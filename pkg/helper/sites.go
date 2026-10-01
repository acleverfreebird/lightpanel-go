package helper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// ValidEmail bounds ACME registration addresses syntactically; the ACME
// server does the real verification.
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

// distroDefaultSite points at the stock virtual host a distribution ships in
// sites-enabled (Debian/Ubuntu layout): Enabled is what the web server
// actually includes, Available is the source file the symlink targets.
type distroDefaultSite struct {
	Enabled   string
	Available string
}

// 发行版默认站点与 nginx 主配置路径。var 仅为让测试能重定向到临时目录。
var (
	distroDefaultSites = map[string]distroDefaultSite{
		"nginx":  {Enabled: "/etc/nginx/sites-enabled/default", Available: "/etc/nginx/sites-available/default"},
		"apache": {Enabled: "/etc/apache2/sites-enabled/000-default.conf", Available: "/etc/apache2/sites-available/000-default.conf"},
	}
	nginxSitesEnabledDir = "/etc/nginx/sites-enabled"
	nginxMainConf        = "/etc/nginx/nginx.conf"
)

// IsDistroDefaultConf reports whether content matches the stock virtual host
// the distribution ships as its default site: it claims the port-80 default
// slot and points at the stock docroot. Anything else sharing the file name
// is an administrator's configuration and must never be removed.
func IsDistroDefaultConf(engine string, content []byte) bool {
	s := string(content)
	switch engine {
	case "nginx":
		return strings.Contains(s, "default_server") && strings.Contains(s, "/var/www/html")
	case "apache":
		return strings.Contains(s, "DocumentRoot /var/www/html")
	}
	return false
}

// TakeOverDefaultSite disables the distribution's stock default vhost when a
// panel site is about to claim port 80. The stock site owns the
// default_server slot, so until it is disabled every request that does not
// match a server_name (IP access, unresolved domains) keeps hitting the
// "Welcome to nginx!" page — the panel site and its index.html never serve
// traffic. Only the sites-enabled entry is removed; sites-available keeps the
// source so an administrator can restore it. Returns a note for the task
// output, empty when there is nothing to do. Failures never block creation.
func TakeOverDefaultSite(engine string, port int) string {
	if port != 80 {
		return ""
	}
	// RHEL 系布局没有 sites-enabled：默认 server 块直接写在主配置里，而
	// 主配置不属于面板的管理范围，只能提示管理员处理。
	if engine == "nginx" && !dirExists(nginxSitesEnabledDir) {
		if data, err := os.ReadFile(nginxMainConf); err == nil && strings.Contains(string(data), "default_server") {
			return "检测到 " + nginxMainConf + " 自带 default_server 站点，IP 访问会继续显示发行版默认页；请在主配置中注释该 server 块，或通过域名访问站点。"
		}
		return ""
	}
	d, ok := distroDefaultSites[engine]
	if !ok {
		return ""
	}
	fi, err := os.Lstat(d.Enabled)
	if err != nil {
		return ""
	}
	if !isDistroDefaultSite(engine, d, fi) {
		return d.Enabled + " 不是发行版默认站点，未改动；IP 访问可能仍由它接管。"
	}
	if err := os.Remove(d.Enabled); err != nil {
		return "停用发行版默认站点失败：" + err.Error()
	}
	return "已停用发行版默认站点（" + d.Enabled + "），新站点接管端口 80 的默认访问。"
}

// isDistroDefaultSite recognizes the stock site either by its content or, for
// symlinks that cannot be read, by pointing back at the distribution's own
// source file in sites-available.
func isDistroDefaultSite(engine string, d distroDefaultSite, fi os.FileInfo) bool {
	if content, err := os.ReadFile(d.Enabled); err == nil && IsDistroDefaultConf(engine, content) {
		return true
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	link, err := os.Readlink(d.Enabled)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(d.Enabled), link)
	}
	return filepath.Clean(link) == d.Available
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

// ChallengeDir 是 ACME HTTP-01 挑战文件的固定存放目录：由 helper（root）
// 写入，权限对网页服务器 worker 可读（目录 0755、文件 0644）。面板与 helper
// 共用该常量，保证生成的站点配置里 alias/Alias 指向一致。
const ChallengeDir = "/var/lib/lightpanel/acme-challenges"

// SSLConf 描述叠加在站点基础配置之上的 HTTPS 设置。Challenge 为 true 时
// 输出中始终包含 ACME 挑战 location（签发与自动续期都依赖它）。
type SSLConf struct {
	Enabled    bool
	ForceHTTPS bool
	CertFile   string
	KeyFile    string
	Challenge  bool
}

// MaxProxyNodes bounds the upstream size of a load-balanced proxy site; the
// generated configuration stays small and reviewable.
const MaxProxyNodes = 16

// ProxyNode is one upstream member of a load-balanced proxy site. Weight 0
// or 1 keeps the engine default; Weight up to 100 maps to nginx "weight=N"
// and Apache "loadfactor=N". Backup members only receive traffic when every
// regular member is down.
type ProxyNode struct {
	Target string `json:"target"`
	Weight int    `json:"weight,omitempty"`
	Backup bool   `json:"backup,omitempty"`
}

// ValidProxyNode bounds one upstream entry: the target must be a valid
// proxy URL and the weight a small positive integer.
func ValidProxyNode(n ProxyNode) bool {
	return ValidProxyTarget(n.Target) && n.Weight >= 0 && n.Weight <= 100
}

// ProxyConf 描述反向代理上游：单节点等价于传统的单一 proxy_pass（允许路
// 径后缀），多节点生成 nginx upstream / Apache balancer 负载均衡组（成员
// 不允许路径）。WebSocket 升级头透传仅 Nginx 支持。
type ProxyConf struct {
	Nodes     []ProxyNode
	WebSocket bool
}

// ValidProxyConf validates the upstream list of a proxy site: at least one
// and at most MaxProxyNodes targets, no duplicates, one shared scheme and —
// for multi-node groups — pathless targets, because upstream members carry
// host:port only.
func ValidProxyConf(engine string, proxy ProxyConf) error {
	if len(proxy.Nodes) == 0 {
		return errors.New("proxy site needs at least one upstream target")
	}
	if len(proxy.Nodes) > MaxProxyNodes {
		return fmt.Errorf("at most %d upstream nodes", MaxProxyNodes)
	}
	seen := make(map[string]bool, len(proxy.Nodes))
	scheme := ""
	for _, n := range proxy.Nodes {
		if !ValidProxyNode(n) {
			return errors.New("invalid upstream target or weight")
		}
		if seen[n.Target] {
			return errors.New("duplicate upstream target: " + n.Target)
		}
		seen[n.Target] = true
		s := strings.SplitN(n.Target, "://", 2)[0]
		if scheme == "" {
			scheme = s
		} else if s != scheme {
			return errors.New("upstream nodes must share one scheme (http or https)")
		}
	}
	if len(proxy.Nodes) > 1 {
		for _, n := range proxy.Nodes {
			if strings.Contains(strings.SplitN(n.Target, "://", 2)[1], "/") {
				return errors.New("multi-node upstream targets must not contain a path")
			}
		}
	}
	if proxy.WebSocket && engine != "nginx" {
		return errors.New("websocket pass-through requires the nginx engine")
	}
	return nil
}

// upstreamAddr strips scheme and path from a validated upstream URL: nginx
// upstream members are bare host:port entries.
func upstreamAddr(target string) string {
	addr := target
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	if i := strings.IndexByte(addr, '/'); i >= 0 {
		addr = addr[:i]
	}
	return addr
}

// upstreamScheme reports the scheme shared by every node (guaranteed by
// ValidProxyConf).
func upstreamScheme(nodes []ProxyNode) string {
	return strings.SplitN(nodes[0].Target, "://", 2)[0]
}

// wsMapVar is the per-site WebSocket connection variable. Nginx variable
// names allow only letters, digits and underscores, so site hyphens become
// underscores; the site prefix keeps maps of different files collision-free.
func wsMapVar(name string) string {
	return "lightpanel_" + strings.ReplaceAll(name, "-", "_") + "_ws"
}

// nginxPrelude renders the http-level blocks a proxy site needs before its
// server blocks: the load-balancing group and the per-site WebSocket map.
// Both server blocks (80/443) share them.
func nginxPrelude(kind, name string, proxy ProxyConf) string {
	if kind != "proxy" {
		return ""
	}
	out := ""
	if len(proxy.Nodes) > 1 {
		out += "upstream lightpanel-" + name + " {\n"
		for _, n := range proxy.Nodes {
			entry := "    server " + upstreamAddr(n.Target)
			if n.Weight > 1 {
				entry += " weight=" + strconv.Itoa(n.Weight)
			}
			if n.Backup {
				entry += " backup"
			}
			out += entry + ";\n"
		}
		out += "}\n"
	}
	if proxy.WebSocket {
		out += "map $http_upgrade " + wsMapVar(name) + " {\n    default upgrade;\n    ''      close;\n}\n"
	}
	return out
}

// proxyPassTarget keeps a single upstream target verbatim (paths allowed)
// and points multi-node sites at their upstream group.
func proxyPassTarget(name string, proxy ProxyConf) string {
	if len(proxy.Nodes) > 1 {
		return upstreamScheme(proxy.Nodes) + "://lightpanel-" + name
	}
	return proxy.Nodes[0].Target
}

// MaxSiteDomains bounds the server_name list of one site.
const MaxSiteDomains = 16

// maxRewriteBytes bounds a custom pseudo-static snippet injected into
// location /.
const maxRewriteBytes = 8192

// indexNamePattern bounds default-document entries to plain file names
// (no paths, no dotfiles).
var indexNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidIndexEntry reports whether name is a usable default-document entry.
func ValidIndexEntry(name string) bool { return indexNamePattern.MatchString(name) }

var redirectCodes = map[int]bool{301: true, 302: true, 307: true, 308: true}

// ValidRewritePreset reports whether preset is a known location / generator.
func ValidRewritePreset(preset string) bool {
	return preset == "" || preset == "spa" || preset == "custom"
}

// Redirect 描述整站跳转：设置后 location / 只输出跳转，站点的其余功能
// （静态文件、代理、伪静态）不再生效。KeepPath 保留原始请求路径。
type Redirect struct {
	Target   string
	Code     int
	KeepPath bool
}

// SiteSpec 是一份站点配置的完整参数：RenderSite 的唯一输入，站点创建与
// 全部在线改写（SSL、上游、域名、默认文档、重定向、伪静态）共用，保证
// 每次改写都从完整状态出发、互不覆盖。
type SiteSpec struct {
	Name   string
	Engine string
	Kind   string // static | proxy
	// Domains 是绑定的域名列表；空列表表示通配站点 "_"。首域名渲染为
	// ServerName（Apache），其余为别名。
	Domains []string
	Port    int
	Root    string
	Proxy   ProxyConf
	SSL     SSLConf
	// Index 是静态站点的默认文档顺序；空使用 index.html index.htm。
	Index []string
	// Redirect 是整站重定向（可选）。
	Redirect *Redirect
	// Rewrite 是静态站点（仅 nginx）location / 的生成方式："spa" 输出
	// try_files 回退到 /index.html（单页应用历史路由），"custom" 输出
	// RewriteBody（config-test 守卫，失败自动回滚）。
	Rewrite     string
	RewriteBody string
}

// validate checks the whole spec; the panel and the helper both run it, so
// neither end can render an invalid configuration.
func (spec SiteSpec) validate() error {
	if !ValidSiteEngine(spec.Engine) {
		return errors.New("engine must be nginx or apache")
	}
	if spec.Kind != "static" && spec.Kind != "proxy" {
		return errors.New("kind must be static or proxy")
	}
	if !ValidPort(spec.Port) {
		return errors.New("port must be 1..65535")
	}
	if len(spec.Domains) > MaxSiteDomains {
		return fmt.Errorf("at most %d domains per site", MaxSiteDomains)
	}
	seen := make(map[string]bool, len(spec.Domains))
	for _, d := range spec.Domains {
		if !ValidServerName(d) {
			return errors.New("invalid server name")
		}
		if seen[d] {
			return errors.New("duplicate domain: " + d)
		}
		seen[d] = true
	}
	if spec.SSL.Enabled && (!ValidPemPath(spec.SSL.CertFile) || !ValidPemPath(spec.SSL.KeyFile)) {
		return errors.New("invalid certificate path")
	}
	if spec.Kind == "proxy" {
		if err := ValidProxyConf(spec.Engine, spec.Proxy); err != nil {
			return err
		}
	} else if !ValidDocumentRoot(spec.Root) {
		return errors.New("invalid site root path")
	}
	if len(spec.Index) > 8 {
		return errors.New("at most 8 default documents")
	}
	for _, name := range spec.Index {
		if !ValidIndexEntry(name) {
			return errors.New("invalid default document: " + name)
		}
	}
	if spec.Redirect != nil {
		if !redirectCodes[spec.Redirect.Code] {
			return errors.New("redirect code must be 301, 302, 307 or 308")
		}
		if !ValidProxyTarget(spec.Redirect.Target) {
			return errors.New("invalid redirect target")
		}
	}
	if !ValidRewritePreset(spec.Rewrite) {
		return errors.New("unknown rewrite preset")
	}
	if spec.Rewrite == "custom" {
		if spec.Engine != "nginx" {
			return errors.New("custom rewrite rules require the nginx engine")
		}
		if len(spec.RewriteBody) > maxRewriteBytes {
			return errors.New("custom rewrite snippet is too large (max 8 KiB)")
		}
		if strings.ContainsRune(spec.RewriteBody, 0) {
			return errors.New("custom rewrite snippet must be UTF-8 text")
		}
	}
	if spec.Rewrite != "" && spec.Kind != "static" {
		return errors.New("rewrite presets only apply to static sites")
	}
	return nil
}

// RenderSite renders the complete configuration file for spec. It is the
// single rendering entry: creation, SSL, proxy editing and the advanced
// settings all converge here, so no rewrite can ever drop a feature another
// rewrite introduced.
func RenderSite(spec SiteSpec) (string, error) {
	if err := spec.validate(); err != nil {
		return "", err
	}
	domains := spec.Domains
	if len(domains) == 0 {
		domains = []string{"_"}
	}
	if spec.Engine == "nginx" {
		return nginxConf(spec, domains), nil
	}
	return apacheConf(spec, domains), nil
}

// redirectTarget renders the redirect operand. nginx 的 $request_uri 与
// Apache 的 $1 都自带/需要路径分隔符（$1 捕获不含前导斜杠），目标自带的
// 尾斜杠先去掉，避免出现双斜杠。
func redirectTarget(r *Redirect, apache bool) string {
	if !r.KeepPath {
		return r.Target
	}
	base := strings.TrimRight(r.Target, "/")
	if apache {
		return base + "/$1"
	}
	return base + "$request_uri"
}

func nginxConf(spec SiteSpec, domains []string) string {
	ssl := spec.SSL
	challenge := ""
	if ssl.Challenge {
		challenge = "    location ^~ /.well-known/acme-challenge/ {\n        alias " + ChallengeDir + "/;\n    }\n"
	}
	body := func(https bool) string {
		out := "server {\n"
		if https {
			out += "    listen 443 ssl;\n    server_name " + strings.Join(domains, " ") + ";\n"
			out += "    ssl_certificate " + ssl.CertFile + ";\n    ssl_certificate_key " + ssl.KeyFile + ";\n"
			out += "    ssl_protocols TLSv1.2 TLSv1.3;\n"
		} else {
			out += "    listen " + strconv.Itoa(spec.Port) + ";\n    server_name " + strings.Join(domains, " ") + ";\n"
		}
		out += challenge
		if spec.Redirect != nil {
			// 整站重定向优先于强制 HTTPS；挑战路径是独立的 ^~ location，
			// 不会被 return 影响。
			out += "    location / {\n        return " + strconv.Itoa(spec.Redirect.Code) + " " + redirectTarget(spec.Redirect, false) + ";\n    }\n"
		} else if https && ssl.Enabled && ssl.ForceHTTPS {
			// 重定向放在 location / 中而不是 server 级 return，保证挑战
			// 路径始终可直达，续期不会被强制跳转打断。
			out += "    location / {\n        return 301 https://$host$request_uri;\n    }\n"
		} else {
			out += nginxLocation(spec)
		}
		out += "}\n"
		return out
	}
	out := ManagedMarker + " — site: " + spec.Name + "\n"
	out += nginxPrelude(spec.Kind, spec.Name, spec.Proxy)
	out += body(false)
	if ssl.Enabled {
		out += body(true)
	}
	return out
}

// indentBody re-indents a custom location body (8 spaces) for embedding.
func indentBody(body string) string {
	lines := strings.Split(strings.Trim(body, "\n"), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "" {
			lines[i] = "        " + strings.TrimRight(line, " \t")
		} else {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

func nginxLocation(spec SiteSpec) string {
	if spec.Kind == "static" {
		index := spec.Index
		if len(index) == 0 {
			index = []string{"index.html", "index.htm"}
		}
		out := "    root " + spec.Root + ";\n    index " + strings.Join(index, " ") + ";\n\n    location / {\n"
		switch spec.Rewrite {
		case "spa":
			out += "        try_files $uri $uri/ /index.html;\n"
		case "custom":
			if body := indentBody(spec.RewriteBody); body != "" {
				out += body + "\n"
			}
		default:
			out += "        try_files $uri $uri/ =404;\n"
		}
		out += "    }\n"
		return out
	}
	proxy := spec.Proxy
	out := "\n    location / {\n        proxy_pass " + proxyPassTarget(spec.Name, proxy) + ";\n"
	if proxy.WebSocket {
		out += "        proxy_http_version 1.1;\n"
		out += "        proxy_set_header Upgrade $http_upgrade;\n"
		out += "        proxy_set_header Connection $" + wsMapVar(spec.Name) + ";\n"
	}
	out += "        proxy_set_header Host $host;\n"
	out += "        proxy_set_header X-Real-IP $remote_addr;\n"
	out += "        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n"
	out += "        proxy_set_header X-Forwarded-Proto $scheme;\n"
	out += "    }\n"
	return out
}

// apacheBalancer renders the shared balancer group once at server level so
// the 80/443 virtual-host pair can reference it without redefining it.
// Members keep their scheme (host:port), weights map to loadfactor and
// backup members to the hot-standby status flag.
func apacheBalancer(name string, nodes []ProxyNode) string {
	out := "<Proxy balancer://lightpanel-" + name + ">\n"
	for _, n := range nodes {
		member := "    BalancerMember " + n.Target
		if n.Weight > 1 {
			member += " loadfactor=" + strconv.Itoa(n.Weight)
		}
		if n.Backup {
			member += " status=+H"
		}
		out += member + "\n"
	}
	return out + "</Proxy>\n"
}

// apachePassTarget keeps a single target verbatim and points multi-node
// sites at their balancer group.
func apachePassTarget(name string, proxy ProxyConf) string {
	if len(proxy.Nodes) > 1 {
		return "balancer://lightpanel-" + name + "/"
	}
	return proxy.Nodes[0].Target
}

func apacheConf(spec SiteSpec, domains []string) string {
	ssl := spec.SSL
	name := spec.Name
	challenge := ""
	if ssl.Challenge {
		challenge = "    Alias /.well-known/acme-challenge/ " + ChallengeDir + "/\n" +
			"    <Directory " + ChallengeDir + ">\n        Require all granted\n    </Directory>\n"
	}
	// 强制 HTTPS 用 RedirectMatch 排除挑战路径；Apache 会先匹配
	// Redirect(Match) 再匹配 Alias，负向前瞻保证挑战请求不受影响。
	forceHTTPS := ""
	if ssl.Enabled && ssl.ForceHTTPS && spec.Redirect == nil && domains[0] != "_" {
		forceHTTPS = "    RedirectMatch permanent ^/(?!\\.well-known/acme-challenge/)(.*)$ https://" + domains[0] + "/$1\n"
	}
	// 显式整站重定向：RedirectMatch + 负向前瞻同样豁免挑战路径；80/443
	// 两个 VirtualHost 都输出（重定向站点在任何入口都只做跳转）。
	siteRedirect := ""
	if spec.Redirect != nil {
		siteRedirect = "    RedirectMatch " + strconv.Itoa(spec.Redirect.Code) + " ^/(?!\\.well-known/acme-challenge/)(.*)$ " + redirectTarget(spec.Redirect, true) + "\n"
	}
	content := ""
	if spec.Kind == "static" {
		content = "    DocumentRoot " + spec.Root + "\n"
		if len(spec.Index) > 0 {
			content += "    DirectoryIndex " + strings.Join(spec.Index, " ") + "\n"
		}
		content += "\n    <Directory " + spec.Root + ">\n        Require all granted\n    </Directory>\n"
	} else {
		content = "\n    ProxyPreserveHost On\n    ProxyPass / " + apachePassTarget(name, spec.Proxy) + "\n    ProxyPassReverse / " + apachePassTarget(name, spec.Proxy) + "\n"
	}
	body := func(https bool) string {
		out := "<VirtualHost *:" + strconv.Itoa(httpPort(spec.Port, https)) + ">\n"
		out += "    ServerName " + domains[0] + "\n"
		if len(domains) > 1 {
			out += "    ServerAlias " + strings.Join(domains[1:], " ") + "\n"
		}
		if https {
			out += "    SSLEngine on\n    SSLCertificateFile " + ssl.CertFile + "\n    SSLCertificateKeyFile " + ssl.KeyFile + "\n"
			out += siteRedirect
		} else {
			out += challenge + forceHTTPS + siteRedirect
		}
		out += content
		out += "</VirtualHost>\n"
		return out
	}
	out := ManagedMarker + " — site: " + name + "\n"
	if spec.Kind == "proxy" && len(spec.Proxy.Nodes) > 1 {
		out += apacheBalancer(name, spec.Proxy.Nodes) + "\n"
	}
	if spec.Port != 80 && spec.Port != 443 {
		out += "Listen " + strconv.Itoa(spec.Port) + "\n\n"
	}
	out += body(false)
	if ssl.Enabled {
		out += body(true)
	}
	return out
}

// SiteConf renders the complete server-block/VirtualHost file for a site.
// kind is "static" or "proxy"; proxy sites ignore root and use proxyTarget.
func SiteConf(engine, kind, name, domain string, port int, root, proxyTarget string) (string, error) {
	proxy := ProxyConf{}
	if kind == "proxy" {
		proxy = ProxyConf{Nodes: []ProxyNode{{Target: proxyTarget}}}
	}
	return SiteConfEx(engine, kind, name, domain, port, root, proxy, SSLConf{})
}

// SiteConfEx 是 SiteConf 的完整版本（SSL + 反向代理）。新增功能一律通过
// SiteSpec + RenderSite；此包装保留给证书签发流程与既有调用方。
func SiteConfEx(engine, kind, name, domain string, port int, root string, proxy ProxyConf, ssl SSLConf) (string, error) {
	domains := []string(nil)
	if domain != "" {
		domains = []string{domain}
	}
	return RenderSite(SiteSpec{
		Name: name, Engine: engine, Kind: kind, Domains: domains,
		Port: port, Root: root, Proxy: proxy, SSL: ssl,
	})
}

// homeStateSuffix 是面板状态目录的 HOME 回退形态（无 /var/lib/lightpanel
// 写权限时面板退回到 <home>/.local/state/lightpanel/acme）。
const homeStateSuffix = ".local/state/lightpanel/acme/certs/"

// ValidPemPath 限制 SSL 证书/私钥只能来自面板自己的证书目录（或该目录不
// 存在时的状态目录回退路径），防止 helper 把任意文件嵌入 nginx 配置。
func ValidPemPath(path string) bool {
	if !ValidAbsPath(path) || !strings.HasSuffix(path, ".pem") {
		return false
	}
	if strings.HasPrefix(path, "/var/lib/lightpanel/acme/certs/") {
		return true
	}
	if strings.HasPrefix(path, "/root/"+homeStateSuffix) {
		return true
	}
	if rest, ok := strings.CutPrefix(path, "/home/"); ok {
		user, sub, found := strings.Cut(rest, "/")
		return found && user != "" && strings.HasPrefix(sub, homeStateSuffix)
	}
	return false
}

func httpPort(port int, https bool) int {
	if https {
		return 443
	}
	return port
}

// ErrConflict is returned when a site, file or container already exists.
var ErrConflict = errors.New("already exists")
