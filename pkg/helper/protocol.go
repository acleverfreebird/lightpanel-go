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
)

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
}

// Response is the helper's verdict. OK=false carries a human-readable Error
// plus any captured command Output.
type Response struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
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
