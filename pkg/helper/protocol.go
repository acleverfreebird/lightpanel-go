// Package helper implements the privileged helper protocol: the panel HTTP
// process runs unprivileged and forwards a small whitelist of root-level
// operations to a separate helper process over a local unix socket. The
// helper re-validates every request against a per-service/per-action ACL and
// peer credentials, so the network-facing process never needs root.
package helper

import (
	"encoding/binary"
	"errors"
	"io"
	"strconv"
)

// Operations. One request carries exactly one operation.
const (
	OpService        = "service"         // unit + whitelisted action, gated by the per-unit ACL
	OpFirewallStatus = "firewall_status" // read-only ufw/firewalld status
	OpFirewall       = "firewall"        // add/remove a port rule
	OpKill           = "kill"            // signal a process by pinned identity
	OpUpdate         = "update"          // verify + install a checksummed update and restart the panel
	OpSite           = "site"            // managed web-site configuration, gated by allow_sites
	OpApp            = "app"             // install or remove a catalog app via the system package manager, gated by allow_apps
	OpDatabase       = "database"        // managed database operations, gated by allow_databases
	OpTerminal       = "terminal"        // relay one root web-terminal PTY, gated by allow_terminal
	OpFile           = "file"            // whole-filesystem file management as root, gated by allow_files
	OpHello          = "hello"           // protocol handshake; no ACL, returns ProtocolVersion
)

// ProtocolVersion is the protocol revision this binary speaks. The helper
// echoes it in every response, and a panel that sees a different value knows
// the running helper process is stale — the panel and helper share one
// binary but restart separately, so an upgrade leaves the old helper image
// running until it is restarted. Without the handshake such a helper fails
// new operations with a bare "unknown operation".
//
// v2: certbot issuance replaced by panel-side ACME; new site actions
// ssl-apply / challenge-set / challenge-clear.
// v3: new terminal operation relaying the web terminal's root PTY.
// v4: database actions extended with one-step provisioning (create-db gains
// user/password/charset/host), backups (backup / backup-list / backup-restore
// / backup-delete) and the SQL console (query).
// v5: new file operation: the whole file manager (browse, download, upload,
// edit, mkdir, rename, delete, chmod) executes as root through the helper,
// so an unprivileged panel no longer fails on root-owned files with 403.
// v6: new site action proxy-apply rewrites a managed proxy site with a
// multi-node upstream list (nginx upstream / Apache balancer) and optional
// WebSocket pass-through.
// v7: new site action conf-apply applies the full site spec (multi-domain
// binding, default documents, whole-site redirect, pseudo-static presets).
// v8: new file actions copy / move / trash / trash-list / trash-restore /
// trash-delete / trash-empty: multi-select batch operations, a clipboard and
// a restorable recycle bin, executing as root in both panel modes.
const ProtocolVersion = 8

// Relay framing for OpTerminal. After the helper answers the terminal request
// with an OK Response, the connection stops speaking JSON: the panel sends
// framed messages (one frame per client keystroke batch or resize) while the
// helper answers with raw PTY output bytes until the shell exits and the
// connection closes (EOF on the panel side). Every frame is 1 type byte, a
// uint32-BE payload length and the payload.
const (
	// FrameInput carries raw keystrokes. File-content relays reuse it for
	// upload/save body chunks (panel→helper only).
	FrameInput = 0x01
	// FrameResize resizes the PTY; its payload is rows and cols as two
	// uint16-BE integers. File relays reject it.
	FrameResize = 0x02
	// FrameCommit ends a file upload/save relay: on receipt the helper
	// finalizes (fsync, rename into place) and answers with the final JSON
	// Response. A connection that ends without FrameCommit aborts the
	// transfer and removes the temporary file, so an interrupted panel can
	// never leave a truncated file behind.
	FrameCommit = 0x03
)

// WriteCommitFrame sends the end-of-transfer marker of a file relay.
func WriteCommitFrame(w io.Writer) error {
	return writeFrame(w, FrameCommit, nil)
}

// FileEntry is one directory-listing row. The helper marshals the listing
// directly into the response body, so this struct defines the on-the-wire
// (and browser-facing) JSON for both helper and panel modes.
type FileEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"is_dir"`
	Regular  bool   `json:"regular"`
	Symlink  bool   `json:"symlink"`
	Size     int64  `json:"size"`
	Mode     string `json:"mode"`
	Modified int64  `json:"modified"`
}

// WriteInputFrame sends one keystroke batch to the relay.
func WriteInputFrame(w io.Writer, data []byte) error {
	return writeFrame(w, FrameInput, data)
}

// WriteResizeFrame sends one PTY resize to the relay.
func WriteResizeFrame(w io.Writer, rows, cols uint16) error {
	return writeFrame(w, FrameResize, []byte{byte(rows >> 8), byte(rows), byte(cols >> 8), byte(cols)})
}

func writeFrame(w io.Writer, kind byte, payload []byte) error {
	head := [5]byte{kind, byte(len(payload) >> 24), byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload))}
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadRelayFrame reads one framed message from the relay stream. The frame
// type and payload are returned; io.EOF at a frame boundary means the relay
// (or the shell behind it) is gone.
func ReadRelayFrame(r io.Reader) (kind byte, payload []byte, err error) {
	var head [5]byte
	if _, err = io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	kind = head[0]
	n := binary.BigEndian.Uint32(head[1:])
	if n > 1<<20 {
		return 0, nil, errors.New("terminal frame too large")
	}
	payload = make([]byte, n)
	if n > 0 {
		if _, err = io.ReadFull(r, payload); err != nil {
			return 0, nil, err
		}
	}
	return kind, payload, nil
}

var serviceActions = map[string]bool{"start": true, "stop": true, "restart": true, "reload": true, "enable": true, "disable": true}

// ValidServiceAction reports whether action is a helper-permitted systemd action.
func ValidServiceAction(action string) bool { return serviceActions[action] }

// Site actions for OpSite. Create and ssl-apply render the whole
// configuration file on the helper side from validated parameters — the
// panel never ships file content.
var siteActions = map[string]bool{
	"create":          true, // kind=static|proxy site; helper writes conf, docroot, symlink, reloads
	"delete":          true, // remove a marker-bearing managed .conf (and its enabled symlink)
	"reload":          true, // reload nginx/apache after out-of-band edits
	"ssl-apply":       true, // rewrite a managed conf with/without HTTPS + reload (config-test guarded)
	"proxy-apply":     true, // rewrite a managed proxy conf with a new upstream list + reload (config-test guarded)
	"conf-apply":      true, // rewrite a managed conf from a full SiteSpec (domains, index, redirect, rewrite) + reload
	"challenge-set":   true, // write one ACME HTTP-01 token file into the fixed challenge dir
	"challenge-clear": true, // remove one ACME token file (empty token clears the directory)
}

// ValidSiteAction reports whether action is a helper-permitted site operation.
func ValidSiteAction(action string) bool { return siteActions[action] }

// App actions for OpApp. "install" and "remove": the helper resolves the
// system package manager itself and rebuilds the full argv from the catalog
// in apps.go — the panel only sends the app name.
var appActions = map[string]bool{"install": true, "remove": true}

// ValidAppAction reports whether action is a helper-permitted app operation.
func ValidAppAction(action string) bool { return appActions[action] }

// Database actions for OpDatabase. As with sites and apps, the helper
// rebuilds every argument (see databases.go) — the panel sends only the
// engine, a validated name and, for user operations, the password. The
// password is transported in the Request (never argv) and is fed to the
// client binary over stdin helper-side, so it never appears in a process list.
var dbActions = map[string]bool{
	"list":           true, // read-only database (and user) listing, with sizes
	"create-db":      true, // create one database, optionally with its user and grants
	"drop-db":        true, // drop one database
	"create-user":    true, // create a local user with a password
	"set-password":   true, // change an existing user's password
	"backup":         true, // dump one database into the managed backup directory
	"backup-list":    true, // list the managed backup files of one database
	"backup-restore": true, // stream one managed backup file back into the database
	"backup-delete":  true, // remove one managed backup file
	"query":          true, // run one SQL batch against one database, return the output
}

// ValidDBAction reports whether action is a helper-permitted database operation.
func ValidDBAction(action string) bool { return dbActions[action] }

// File actions for OpFile. Every action carries only validated absolute
// paths; the helper re-validates them and re-derives all syscall arguments,
// exactly like sites/apps/databases rebuild their argv. list/read/mkdir/
// rename/delete/chmod are ordinary request/response calls; fetch/store/save
// switch the connection to a content relay after the handshake (see
// FrameCommit and Client.Relay).
var fileActions = map[string]bool{
	"list":          true, // directory listing as the JSON response body
	"read":          true, // editor read: UTF-8 text up to MaxEditBytes, JSON response body
	"fetch":         true, // download: relay streams the regular file's raw bytes
	"store":         true, // upload: relay receives raw bytes, never overwrites (O_EXCL)
	"save":          true, // editor save: relay receives bytes, atomic tmp+rename preserving owner/mode
	"mkdir":         true, // MkdirAll semantics
	"rename":        true, // renameat2 RENAME_NOREPLACE from Path to To
	"delete":        true, // remove, or RemoveAll when Recursive is set
	"chmod":         true, // three octal digits in Mode, regular files and directories only
	"copy":          true, // copy the entry Path into the directory To, never overwriting
	"move":          true, // move Path into To (rename, or copy+remove across devices)
	"trash":         true, // move Path into the recycle bin, restorable
	"trash-list":    true, // list the recycle bin's restorable entries
	"trash-restore": true, // put the entry Path (a trash id) back at its original location
	"trash-delete":  true, // permanently remove the entry Path (a trash id) from the bin
	"trash-empty":   true, // clear the whole recycle bin
}

// ValidFileAction reports whether action is a helper-permitted file operation.
func ValidFileAction(action string) bool { return fileActions[action] }

// IsFileRelayAction reports whether the file action transfers content over
// the relayed connection instead of the one-shot JSON exchange.
func IsFileRelayAction(action string) bool {
	return action == "fetch" || action == "store" || action == "save"
}

// MaxEditBytes is the online editor's size cap, shared by the panel (which
// caps the request body) and the helper (which enforces it independently).
const MaxEditBytes = 1 << 20

// Request is one privileged operation. Fields not relevant to Op are ignored.
type Request struct {
	Op        string `json:"op"`
	Unit      string `json:"unit,omitempty"`
	Action    string `json:"action,omitempty"`
	Engine    string `json:"engine,omitempty"`
	Port      string `json:"port,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	PID       int    `json:"pid,omitempty"`
	Signal    int    `json:"signal,omitempty"`
	StartTime string `json:"start_time,omitempty"`
	Dir       string `json:"dir,omitempty"`
	// OpSite fields. Path is only honored for delete and must pass
	// IsManagedConfPath plus the managed-marker check on the helper side.
	Path        string `json:"path,omitempty"`
	Site        string `json:"site,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Domain      string `json:"domain,omitempty"`
	Root        string `json:"root,omitempty"`
	ProxyTarget string `json:"proxy_target,omitempty"`
	// OpSite ssl-apply fields. SSLOn/ForceHTTPS switch the generated
	// configuration; CertFile/KeyFile must pass ValidPemPath and exist.
	SSLOn      bool   `json:"ssl_on,omitempty"`
	ForceHTTPS bool   `json:"force_https,omitempty"`
	CertFile   string `json:"cert_file,omitempty"`
	KeyFile    string `json:"key_file,omitempty"`
	// OpSite proxy-apply fields. ProxyNodes carries the upstream node list
	// (every entry re-validated helper-side via ValidProxyNode/ValidProxyConf,
	// which also honors ProxyTarget as a single-node fallback); WebSocket
	// enables upgrade-header pass-through (nginx only).
	ProxyNodes []ProxyNode `json:"proxy_nodes,omitempty"`
	WebSocket  bool        `json:"websocket,omitempty"`
	// OpSite conf-apply fields. Domains rebinds server_name (each entry
	// validated, deduplicated, ≤ MaxSiteDomains); Index sets the default
	// documents of static sites; Redirect*/Rewrite* carry the whole-site
	// redirect and pseudo-static preset (all re-validated via SiteSpec).
	Domains          []string `json:"domains,omitempty"`
	Index            []string `json:"index,omitempty"`
	RedirectTarget   string   `json:"redirect_target,omitempty"`
	RedirectCode     int      `json:"redirect_code,omitempty"`
	RedirectKeepPath bool     `json:"redirect_keep_path,omitempty"`
	Rewrite          string   `json:"rewrite,omitempty"`
	RewriteBody      string   `json:"rewrite_body,omitempty"`
	// OpSite ACME HTTP-01 challenge fields. Token must match the ACME token
	// alphabet; Auth is the key authorization written as the file content.
	Token string `json:"token,omitempty"`
	Auth  string `json:"auth,omitempty"`
	// OpApp fields. App must be a catalog key from apps.go; the helper
	// re-derives the package name and every argument.
	App string `json:"app,omitempty"`
	// OpDatabase fields. DB must be a database engine key from databases.go,
	// Name a validated database/user name and Password a validated password
	// used only for user operations. The helper re-derives every argv element
	// and feeds the password over stdin, never argv.
	DB       string `json:"db,omitempty"`
	Name     string `json:"name,omitempty"`
	Password string `json:"password,omitempty"`
	// One-step database provisioning (create-db). User defaults to Name when
	// empty; Charset must pass ValidDBCharset for the engine (empty keeps the
	// engine default) and Host is the MySQL account host (validated, escaped).
	User    string `json:"user,omitempty"`
	Charset string `json:"charset,omitempty"`
	Host    string `json:"host,omitempty"`
	// Backups: File must pass ValidBackupFile for the engine+name pair (the
	// helper re-parses it, so it can never escape the backup directory).
	File string `json:"file,omitempty"`
	// SQL console: one batch of statements for the named database. Size-capped
	// and fed to the client over stdin.
	SQL string `json:"sql,omitempty"`
	// OpFile fields. Path/To are absolute validated paths (To is the rename
	// destination). Offset paginates the listing; Recursive switches delete to
	// RemoveAll; Mode is the three-octal-digit chmod target (no setuid/setgid).
	To        string `json:"to,omitempty"`
	Offset    int    `json:"offset,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
	Mode      uint32 `json:"mode,omitempty"`
}

// Response is the helper's verdict. OK=false carries a human-readable Error
// plus any captured command Output.
type Response struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
	// Version is the helper's protocol version, set on every response.
	// Absent (0) means the helper predates the handshake entirely.
	Version int `json:"version,omitempty"`
	// Code is an HTTP-style status hint for OpFile failures (404 not found,
	// 403 permission, 409 exists, 413 too large, 400 invalid). The panel maps
	// it onto its own fileError statuses so the browser sees the same codes as
	// in direct-execution mode. Zero means "no hint": callers fall back to 502.
	Code int `json:"code,omitempty"`
}

// ErrHelper is wrapped into every failure returned to the panel so callers
// can distinguish helper errors from local ones.
var ErrHelper = errors.New("privileged helper operation failed")

// SanitizeDir bounds what OpUpdate accepts as a staging directory: an
// absolute, cleaned path without traversal components. The helper adds
// ownership and permission checks on top.
func SanitizeDir(p string) (string, error) {
	if !ValidAbsPath(p) {
		return "", errors.New("staging directory must be an absolute cleaned path")
	}
	return p, nil
}

// ValidPortString mirrors the panel-side validation so both ends reject the
// same inputs before any command is built.
func ValidPortString(s string) bool {
	p, err := strconv.Atoi(s)
	return err == nil && p >= 1 && p <= 65535 && strconv.Itoa(p) == s
}
