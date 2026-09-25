package helper

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ManagedMarker marks configuration files this module created, so destructive
// actions only ever touch objects the panel owns. Shared by the panel and the
// helper so both ends agree on the same objects.
const ManagedMarker = "# managed by lightpanel"

var (
	siteNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	imageRefPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,127}(:[a-zA-Z0-9._-]{1,64})?(@sha256:[a-f0-9]{64})?$`)
	proxyPattern    = regexp.MustCompile(`^https?://[a-zA-Z0-9._-]{1,253}(:[0-9]{1,5})?(/[a-zA-Z0-9._/-]{0,128})?$`)
	emailPattern    = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@[A-Za-z0-9.-]{1,253}\.[A-Za-z]{2,24}$`)
)

// ValidSiteName bounds site identifiers: they become file names, container
// names and docroot directory names, so the alphabet stays small.
func ValidSiteName(name string) bool { return siteNamePattern.MatchString(name) }

// ValidImageRef accepts a docker image reference and nothing that could be
// mistaken for extra argv or shell metacharacters.
func ValidImageRef(ref string) bool {
	return imageRefPattern.MatchString(ref) && !strings.Contains(ref, "..")
}

// ValidProxyTarget accepts an http(s) upstream URL without userinfo, query or
// fragment; the upstream host is re-validated label by label.
func ValidProxyTarget(target string) bool {
	if !proxyPattern.MatchString(target) {
		return false
	}
	host := target
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, ":/"); i >= 0 {
		host = host[:i]
	}
	if !ValidServerName(host) {
		return false
	}
	// validate the optional port numerically
	rest := target
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		if !ValidPortString(rest[i+1:]) {
			return false
		}
	}
	return true
}

// ValidEmail bounds ACME registration addresses syntactically; certbot does
// the real verification against the CA.
func ValidEmail(email string) bool { return len(email) <= 254 && emailPattern.MatchString(email) }

// ValidServerName accepts DNS hostnames, one leading wildcard label and the
// nginx catch-all "_". It is a syntactic gate only; everything ends up inside
// a generated config file without shell involvement.
func ValidServerName(name string) bool {
	if name == "_" {
		return true
	}
	name = strings.TrimPrefix(name, "*.")
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
			if !ok {
				return false
			}
			if (i == 0 || i == len(label)-1) && c == '-' {
				return false
			}
		}
	}
	return true
}

func ValidPort(p int) bool { return p >= 1 && p <= 65535 }

// ValidSiteEngine reports whether engine names a web server this module can
// configure natively (docker containers are handled by the panel directly).
func ValidSiteEngine(engine string) bool { return engine == "nginx" || engine == "apache" }

// ValidDocumentRoot accepts an absolute, cleaned unix path usable as a web
// root: no traversal, no backslashes, at least one path segment.
func ValidDocumentRoot(root string) bool {
	if len(root) < 2 || !strings.HasPrefix(root, "/") || strings.ContainsAny(root, "\\\x00") || strings.HasSuffix(root, "/") {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(root, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// ManagedConfDirs bounds which configuration files the site module will ever
// read, list or delete. Everything outside is invisible to it.
var ManagedConfDirs = []string{
	"/etc/nginx/sites-enabled/",
	"/etc/nginx/sites-available/",
	"/etc/nginx/conf.d/",
	"/etc/apache2/sites-enabled/",
	"/etc/apache2/sites-available/",
	"/etc/apache2/conf.d/",
	"/etc/httpd/conf.d/",
}

// IsManagedConfPath reports whether id is a .conf file inside one of the
// managed directories, lexically clean.
func IsManagedConfPath(id string) bool {
	if !strings.HasSuffix(id, ".conf") || !strings.HasPrefix(id, "/") {
		return false
	}
	for _, dir := range ManagedConfDirs {
		if strings.HasPrefix(id, dir) && !strings.ContainsAny(id, "\\\x00") && !strings.Contains(id, "..") {
			return true
		}
	}
	return false
}

// EngineUnits lists the systemd unit candidates per site engine, in
// distribution preference order.
var EngineUnits = map[string][]string{"nginx": {"nginx"}, "apache": {"apache2", "httpd"}}

// NativeConfPath picks the distribution-specific location: Debian/Ubuntu use
// sites-available + a symlink in sites-enabled; RHEL-family and default
// installs use a single file in conf.d.
func NativeConfPath(engine, name string) (conf, enabled string) {
	if engine == "nginx" {
		if dirExists("/etc/nginx/sites-enabled") {
			return "/etc/nginx/sites-available/" + name + ".conf", "/etc/nginx/sites-enabled/" + name + ".conf"
		}
		return "/etc/nginx/conf.d/" + name + ".conf", ""
	}
	if dirExists("/etc/apache2/sites-enabled") {
		return "/etc/apache2/sites-available/" + name + ".conf", "/etc/apache2/sites-enabled/" + name + ".conf"
	}
	return "/etc/httpd/conf.d/" + name + ".conf", ""
}

func dirExists(path string) bool {
	s, err := os.Stat(path)
	return err == nil && s.IsDir()
}

const PlaceholderIndex = `<!doctype html>
<html lang="zh-CN">
<head><meta charset="utf-8"><title>站点已就绪</title></head>
<body style="font-family:sans-serif;display:grid;place-items:center;min-height:100vh">
  <div style="text-align:center">
    <h1>站点已就绪</h1>
    <p>此页面由 LightPanel 创建。请将网站文件上传到站点目录。</p>
  </div>
</body>
</html>
`

// SiteConf renders the complete server-block/VirtualHost file for a site.
// kind is "static" or "proxy"; proxy sites ignore root and use proxyTarget.
func SiteConf(engine, kind, name, domain string, port int, root, proxyTarget string) (string, error) {
	if !ValidSiteEngine(engine) {
		return "", errors.New("engine must be nginx or apache")
	}
	if kind != "static" && kind != "proxy" {
		return "", errors.New("kind must be static or proxy")
	}
	if !ValidPort(port) {
		return "", errors.New("port must be 1..65535")
	}
	if domain == "" {
		domain = "_"
	}
	if !ValidServerName(domain) {
		return "", errors.New("invalid server name")
	}
	if kind == "proxy" {
		if !ValidProxyTarget(proxyTarget) {
			return "", errors.New("invalid proxy target")
		}
	} else if !ValidDocumentRoot(root) {
		return "", errors.New("invalid site root path")
	}
	body := ManagedMarker + " — site: " + name + "\n"
	if engine == "nginx" {
		body += "server {\n    listen " + strconv.Itoa(port) + ";\n    server_name " + domain + ";\n"
		if kind == "static" {
			body += "    root " + root + ";\n    index index.html index.htm;\n\n    location / {\n        try_files $uri $uri/ =404;\n    }\n"
		} else {
			body += "\n    location / {\n        proxy_pass " + proxyTarget + ";\n        proxy_set_header Host $host;\n        proxy_set_header X-Real-IP $remote_addr;\n        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n        proxy_set_header X-Forwarded-Proto $scheme;\n    }\n"
		}
		body += "}\n"
		return body, nil
	}
	if port != 80 && port != 443 {
		body += "Listen " + strconv.Itoa(port) + "\n\n"
	}
	body += "<VirtualHost *:" + strconv.Itoa(port) + ">\n    ServerName " + domain + "\n"
	if kind == "static" {
		body += "    DocumentRoot " + root + "\n\n    <Directory " + root + ">\n        Require all granted\n    </Directory>\n"
	} else {
		body += "\n    ProxyPreserveHost On\n    ProxyPass / " + proxyTarget + "\n    ProxyPassReverse / " + proxyTarget + "\n"
	}
	body += "</VirtualHost>\n"
	return body, nil
}

// ErrConflict is returned when a site, file or container already exists.
var ErrConflict = errors.New("already exists")
