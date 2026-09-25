// Package helper implements the privileged helper protocol: the panel HTTP
// process runs unprivileged and forwards a small whitelist of root-level
// operations to a separate helper process over a local unix socket. The
// helper re-validates every request against a per-service/per-action ACL and
// peer credentials, so the network-facing process never needs root.
package helper

import (
	"errors"
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
	OpApp            = "app"             // install a catalog app via the system package manager, gated by allow_apps
	OpDatabase       = "database"        // managed database operations, gated by allow_databases
	OpHello          = "hello"           // protocol handshake; no ACL, returns ProtocolVersion
)

// ProtocolVersion is the protocol revision this binary speaks. The helper
// echoes it in every response, and a panel that sees a different value knows
// the running helper process is stale — the panel and helper share one
// binary but restart separately, so an upgrade leaves the old helper image
// running until it is restarted. Without the handshake such a helper fails
// new operations with a bare "unknown operation".
const ProtocolVersion = 1

var serviceActions = map[string]bool{"start": true, "stop": true, "restart": true, "reload": true, "enable": true, "disable": true}

// ValidServiceAction reports whether action is a helper-permitted systemd action.
func ValidServiceAction(action string) bool { return serviceActions[action] }

// Site actions for OpSite. Create renders the whole configuration file on the
// helper side from validated parameters — the panel never ships file content.
var siteActions = map[string]bool{
	"create":      true, // kind=static|proxy site; helper writes conf, docroot, symlink, reloads
	"delete":      true, // remove a marker-bearing managed .conf (and its enabled symlink)
	"reload":      true, // reload nginx/apache after out-of-band edits
	"issue-cert":  true, // certbot certificate issuance for one domain
	"cert-status": true, // read-only `certbot certificates` listing
}

// ValidSiteAction reports whether action is a helper-permitted site operation.
func ValidSiteAction(action string) bool { return siteActions[action] }

// App actions for OpApp. The only action is "install": the helper resolves
// the system package manager itself and rebuilds the full argv from the
// catalog in apps.go — the panel only sends the app name.
var appActions = map[string]bool{"install": true}

// ValidAppAction reports whether action is a helper-permitted app operation.
func ValidAppAction(action string) bool { return appActions[action] }

// Database actions for OpDatabase. As with sites and apps, the helper
// rebuilds every argument (see databases.go) — the panel sends only the
// engine, a validated name and, for user operations, the password. The
// password is transported in the Request (never argv) and is fed to the
// client binary over stdin helper-side, so it never appears in a process list.
var dbActions = map[string]bool{
	"list":         true, // read-only database (and user) listing
	"create-db":    true, // create one database
	"drop-db":      true, // drop one database
	"create-user":  true, // create a local user with a password
	"set-password": true, // change an existing user's password
}

// ValidDBAction reports whether action is a helper-permitted database operation.
func ValidDBAction(action string) bool { return dbActions[action] }

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
	Email       string `json:"email,omitempty"`
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
