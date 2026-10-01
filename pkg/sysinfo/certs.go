package sysinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lightpanel/pkg/certs"
	"lightpanel/pkg/helper"
)

// certIssuer abstracts the ACME client so tests can replace the network
// round-trip while the orchestration under test stays identical.
type certIssuer interface {
	Issue(ctx context.Context, domain, email string, staging bool, sink certs.ChallengeSink) (chainPEM, keyPEM []byte, err error)
}

// CertManager issues and renews Let's Encrypt certificates with the built-in
// ACME client (pkg/certs): the panel performs the ACME round-trip itself,
// plants HTTP-01 challenge files through the privileged helper and lets the
// helper rewrite the managed site configuration for HTTPS. No certbot or
// other external tool is involved.
type CertManager struct {
	sites *SiteManager
	Tasks *TaskManager
	Store *certs.Store

	taskMu sync.Mutex
	// issueMu serializes issuance: renewal and manual requests for the same
	// domain must never run interleaved.
	issueMu sync.Mutex
	// loadSite and newIssuer are seams for tests.
	loadSite  func(id string) (*managedSite, error)
	newIssuer func(store *certs.Store, log certs.Progress) certIssuer
}

func NewCertManager(sites *SiteManager, tasks *TaskManager, store *certs.Store) *CertManager {
	return &CertManager{sites: sites, Tasks: tasks, Store: store, loadSite: loadManagedSite}
}

func (m *CertManager) issuer(log certs.Progress) certIssuer {
	if m.newIssuer != nil {
		return m.newIssuer(m.Store, log)
	}
	return &certs.Issuer{Store: m.Store, Log: log}
}

func (m *CertManager) TaskCenter() *TaskManager {
	if m.Tasks != nil {
		return m.Tasks
	}
	m.taskMu.Lock()
	defer m.taskMu.Unlock()
	if m.Tasks == nil {
		m.Tasks = &TaskManager{}
	}
	return m.Tasks
}

// ---- managed site lookup ----

// managedSite is a site parsed straight from its (marker-bearing) config
// file. Only these sites support one-click SSL and reverse-proxy editing:
// the configuration template is fully owned by the panel.
type managedSite struct {
	ID     string
	Engine string
	Kind   string
	Domain string
	Port   int
	Root   string
	// ProxyPass is the raw proxy_pass/ProxyPass target (for multi-node sites
	// the balancer URL); ProxyNodes is the expanded upstream list and
	// WebSocket the upgrade-header flag. Rewrites carry all three so the
	// regenerated configuration keeps the site's proxy topology.
	ProxyNodes []helper.ProxyNode
	WebSocket  bool
	ProxyPass  string
	// Advanced settings parsed back from the configuration; rewrites carry
	// them so no feature is dropped when another one changes.
	Index            []string
	RedirectCode     int
	RedirectTarget   string
	RedirectKeepPath bool
	Rewrite          string
	RewriteBody      string
	SSL              bool
	SiteName         string
	ServerName       string
}

// loadManagedSite re-reads and re-parses the site configuration identified
// by id (a managed .conf path), so the SSL flow always works from the file
// on disk rather than from client-supplied form fields.
func loadManagedSite(id string) (*managedSite, error) {
	if !isManagedConfPath(id) {
		return nil, errors.New("configuration path is outside the managed directories")
	}
	target := id
	if fi, err := os.Lstat(id); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if link, err := os.Readlink(id); err == nil {
			target = link
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, err
	}
	if len(data) > 512<<10 || !fileManaged(target) {
		return nil, errors.New("拒绝改写：配置文件不是由 LightPanel 创建；仅面板创建的站点支持一键 SSL")
	}
	conf := string(data)
	var engine string
	switch {
	case strings.HasPrefix(target, "/etc/nginx/"):
		engine = "nginx"
	case strings.HasPrefix(target, "/etc/apache2/"), strings.HasPrefix(target, "/etc/httpd/"):
		engine = "apache"
	default:
		return nil, errors.New("unsupported engine path")
	}
	site := &managedSite{ID: id, Engine: engine, Port: 80}
	var locationBody string
	if engine == "nginx" {
		upstreams := parseNginxUpstreams(conf)
		blocks := parseNginxServers(conf)
		if len(blocks) == 0 {
			return nil, errors.New("no server block found in configuration")
		}
		b := blocks[0]
		site.SSL = b.ssl
		site.Root = b.root
		site.ProxyPass = b.proxyPass
		site.WebSocket = b.proxyWS
		site.Index = b.index
		site.RedirectCode = b.redirectCode
		site.RedirectTarget = b.redirectTarget
		site.RedirectKeepPath = b.redirectKeepPath
		if up := upstreamFor(upstreams, b.proxyPass); up != nil {
			site.ProxyNodes = up.servers
		} else if b.proxyPass != "" {
			site.ProxyNodes = []helper.ProxyNode{{Target: b.proxyPass}}
		}
		site.ServerName = strings.Join(b.serverNames, " ")
		site.Domain = firstRealDomain(b.serverNames)
		locationBody = extractNginxLocationBody(conf)
		for _, p := range b.ports {
			if p != 443 {
				site.Port = p
				break
			}
		}
	} else {
		vhosts := parseApacheVHosts(conf)
		if len(vhosts) == 0 {
			return nil, errors.New("no VirtualHost found in configuration")
		}
		v := vhosts[0]
		site.SSL = v.ssl
		site.Root = v.documentRoot
		site.ProxyPass = v.proxyPass
		site.ProxyNodes = v.proxyNodes
		site.Index = v.directoryIndex
		site.RedirectCode = v.redirectCode
		site.RedirectTarget = v.redirectTarget
		site.RedirectKeepPath = v.redirectKeepPath
		names := append([]string{v.serverName}, v.aliases...)
		site.ServerName = strings.Join(names, " ")
		site.Domain = firstRealDomain(names)
		for _, p := range vhostPorts(v.addr) {
			if p != 443 {
				site.Port = p
				break
			}
		}
	}
	if site.Domain == "" {
		return nil, errors.New("站点配置中没有具体域名（_ 或通配符不能签发证书）")
	}
	if site.ProxyPass != "" {
		site.Kind = "proxy"
	} else {
		site.Kind = "static"
		if site.Root == "" {
			return nil, errors.New("站点配置缺少 root/DocumentRoot")
		}
	}
	if engine == "nginx" {
		site.Rewrite, site.RewriteBody = nginxRewriteOf(site.Kind, site.RedirectTarget, locationBody)
	}
	// 站点名取自配置文件名（与创建站点时的名称一致），ssl-apply 据此定位
	// 配置文件；不要从域名派生，两者可能完全不同。
	site.SiteName = strings.TrimSuffix(filepath.Base(target), ".conf")
	return site, nil
}

func firstRealDomain(names []string) string {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || name == "_" || strings.HasPrefix(name, "*.") {
			continue
		}
		if validServerName(name) {
			return strings.ToLower(name)
		}
	}
	return ""
}

// ---- privileged operations (helper in least-privilege mode, direct as root) ----

// proxyConfOf rebuilds the upstream settings of a parsed site so any rewrite
// (SSL apply, renewal) preserves the existing proxy topology.
func proxyConfOf(site *managedSite) helper.ProxyConf {
	if site.Kind != "proxy" {
		return helper.ProxyConf{}
	}
	if len(site.ProxyNodes) > 0 {
		return helper.ProxyConf{Nodes: site.ProxyNodes, WebSocket: site.WebSocket}
	}
	return helper.ProxyConf{Nodes: []helper.ProxyNode{{Target: site.ProxyPass}}}
}

// siteDomains splits the parsed server names into the concrete domain list
// (the "_" placeholder is not a domain).
func siteDomains(site *managedSite) []string {
	var domains []string
	for _, name := range strings.Fields(site.ServerName) {
		if name != "_" {
			domains = append(domains, name)
		}
	}
	return domains
}

// specOf rebuilds the full SiteSpec of a parsed site so any rewrite keeps
// every feature the configuration currently carries (domains, default
// documents, redirect, pseudo-static, upstream, SSL).
func specOf(site *managedSite, ssl helper.SSLConf) helper.SiteSpec {
	spec := helper.SiteSpec{
		Name: site.SiteName, Engine: site.Engine, Kind: site.Kind,
		Domains: siteDomains(site), Port: site.Port, Root: site.Root,
		Proxy: proxyConfOf(site), SSL: ssl,
		Index: site.Index, Rewrite: site.Rewrite, RewriteBody: site.RewriteBody,
	}
	if site.RedirectTarget != "" {
		spec.Redirect = &helper.Redirect{
			Target: site.RedirectTarget, Code: site.RedirectCode, KeepPath: site.RedirectKeepPath,
		}
	}
	return spec
}

// applySSL rewrites the site configuration through the helper's ssl-apply
// action (config-test guarded, rollback on failure). The full site spec
// travels with the request so the re-rendered file keeps every other
// feature. In root mode the same rendering and guard runs locally.
func (m *CertManager) applySSL(ctx context.Context, site *managedSite, ssl helper.SSLConf) error {
	req := helper.Request{
		Op: helper.OpSite, Action: "ssl-apply",
		Engine: site.Engine, Site: site.SiteName, Kind: site.Kind,
		Domain: site.Domain, Port: strconv.Itoa(site.Port),
		Root: site.Root, ProxyTarget: site.ProxyPass,
		ProxyNodes: site.ProxyNodes, WebSocket: site.WebSocket,
		Domains: siteDomains(site), Index: site.Index,
		RedirectTarget: site.RedirectTarget, RedirectCode: site.RedirectCode,
		RedirectKeepPath: site.RedirectKeepPath,
		Rewrite:          site.Rewrite, RewriteBody: site.RewriteBody,
		SSLOn: ssl.Enabled, ForceHTTPS: ssl.ForceHTTPS,
		CertFile: ssl.CertFile, KeyFile: ssl.KeyFile,
	}
	if out, routed, err := privileged(ctx, req); routed {
		if err != nil {
			return fmt.Errorf("%w: %s", err, helper.TrimOutput(out))
		}
		return nil
	}
	return m.rewriteManagedDirect(ctx, site, specOf(site, ssl))
}

// currentSSLConf reconstructs the SSL state of a managed site from the panel
// certificate store, so rewrites (proxy editing) never drop an existing
// HTTPS block. A site whose configuration has HTTPS but whose certificate is
// unknown to the panel (issued externally) is refused instead of silently
// downgraded.
func (m *CertManager) currentSSLConf(site *managedSite) (helper.SSLConf, error) {
	if !site.SSL {
		return helper.SSLConf{Challenge: true}, nil
	}
	items, err := m.Store.List()
	if err == nil {
		for _, item := range items {
			if item.Domain != site.Domain {
				continue
			}
			certPath, keyPath := m.Store.Paths(site.Domain)
			return helper.SSLConf{
				Enabled: true, ForceHTTPS: item.ForceHTTPS, Challenge: true,
				CertFile: certPath, KeyFile: keyPath,
			}, nil
		}
	}
	return helper.SSLConf{}, errors.New("站点配置已启用 HTTPS，但面板没有该域名的证书记录（可能由外部工具签发），无法自动改写；请通过「文件」编辑站点配置")
}

// rewriteManagedDirect is the root-panel twin of the helper's siteApply:
// re-render the full spec, install atomically, config-test guard with
// rollback, then reload.
func (m *CertManager) rewriteManagedDirect(ctx context.Context, site *managedSite, spec helper.SiteSpec) error {
	if !isManagedConfPath(site.ID) {
		return errors.New("configuration path is outside the managed directories")
	}
	current, err := os.ReadFile(site.ID)
	if err != nil {
		return err
	}
	if len(current) > 512<<10 || !fileManaged(site.ID) {
		return errors.New("refusing to rewrite: configuration was not created by lightpanel")
	}
	content, err := helper.RenderSite(spec)
	if err != nil {
		return err
	}
	if err := writeConfFile(site.ID, content); err != nil {
		return err
	}
	if out, err := configTest(ctx, site.Engine, m.sites.RunTimeout); err != nil {
		_ = writeConfFile(site.ID, string(current))
		return fmt.Errorf("配置测试失败，已恢复原配置：%w：%s", err, helper.TrimOutput(out))
	}
	// HTTP-01 挑战与站点访问共用 80 端口：发行版默认站点还占着默认槽位
	// 时一并停用（best-effort，不阻塞改写）。
	helper.TakeOverDefaultSite(site.Engine, site.Port)
	if _, err := m.sites.reloadEngine(ctx, site.Engine); err != nil {
		return fmt.Errorf("配置已写入，但重载 %s 失败：%w", site.Engine, err)
	}
	return nil
}

// parseProxyNodes parses the JSON upstream list from the form. Entries are
// validated here and re-validated independently by the helper.
func parseProxyNodes(raw string) ([]helper.ProxyNode, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("至少保留一个上游节点")
	}
	var nodes []helper.ProxyNode
	if err := json.Unmarshal([]byte(raw), &nodes); err != nil {
		return nil, errors.New("节点数据格式错误")
	}
	if err := helper.ValidProxyConf("nginx", helper.ProxyConf{Nodes: nodes}); err != nil {
		return nil, err
	}
	return nodes, nil
}

// SiteProxy 保存反向代理站点的上游设置（节点增删、权重、备用、WebSocket）。
// 改写从磁盘上的配置重新出发并保留其 SSL 状态；仅面板创建的托管站点支持。
func (m *CertManager) SiteProxy(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Error(w, "site id is required", 400)
		return
	}
	nodes, err := parseProxyNodes(r.FormValue("nodes"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	websocket := r.FormValue("websocket") == "true"
	site, err := m.loadSite(id)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if site.Kind != "proxy" {
		http.Error(w, "仅反向代理站点支持上游设置", 400)
		return
	}
	if websocket && site.Engine != "nginx" {
		http.Error(w, "WebSocket 透传仅支持 Nginx 站点", 400)
		return
	}
	ssl, err := m.currentSSLConf(site)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	req := helper.Request{
		Op: helper.OpSite, Action: "proxy-apply",
		Engine: site.Engine, Site: site.SiteName, Kind: "proxy",
		Domain: site.Domain, Port: strconv.Itoa(site.Port),
		ProxyNodes: nodes, WebSocket: websocket,
		Domains: siteDomains(site),
		SSLOn:   ssl.Enabled, ForceHTTPS: ssl.ForceHTTPS,
		CertFile: ssl.CertFile, KeyFile: ssl.KeyFile,
	}
	if out, routed, err := privileged(r.Context(), req); routed {
		if err != nil {
			commandError(w, out, err)
			return
		}
		JSON(w, map[string]string{"message": "反向代理设置已保存，" + site.Engine + " 配置已重载"})
		return
	}
	spec := specOf(site, ssl)
	spec.Proxy = helper.ProxyConf{Nodes: nodes, WebSocket: websocket}
	if err := m.rewriteManagedDirect(r.Context(), site, spec); err != nil {
		commandError(w, "", err)
		return
	}
	JSON(w, map[string]string{"message": "反向代理设置已保存，" + site.Engine + " 配置已重载"})
}

// parseDomainList parses the JSON domain list from the form; an empty list
// means "bind-all" ("_"). Entries are lowercased, deduplicated and validated
// (the helper re-validates independently).
func parseDomainList(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var domains []string
	if err := json.Unmarshal([]byte(raw), &domains); err != nil {
		return nil, errors.New("域名数据格式错误")
	}
	var out []string
	seen := map[string]bool{}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || d == "_" {
			continue
		}
		if !validServerName(d) {
			return nil, errors.New("无效域名: " + d)
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) > helper.MaxSiteDomains {
		return nil, fmt.Errorf("每个站点最多绑定 %d 个域名", helper.MaxSiteDomains)
	}
	return out, nil
}

// parseIndexLines splits the default-document textarea into entries (one per
// line, comma allowed).
func parseIndexLines(raw string) []string {
	var out []string
	for _, line := range strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }) {
		if name := strings.TrimSpace(line); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// siteScanDirs lists the configuration directories the domain conflict scan
// walks, per engine. A var so tests can point it at temporary directories.
var siteScanDirs = map[string][]string{
	"nginx":  {"/etc/nginx/sites-enabled/", "/etc/nginx/conf.d/"},
	"apache": {"/etc/apache2/sites-enabled/", "/etc/apache2/conf.d/", "/etc/httpd/conf.d/"},
}

// domainOverlap reports whether two hosts would fight over the same
// requests: exact match or one covered by the other's wildcard.
func domainOverlap(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if a == b {
		return true
	}
	if base, ok := strings.CutPrefix(a, "*."); ok {
		return strings.HasSuffix(b, "."+base)
	}
	if base, ok := strings.CutPrefix(b, "*."); ok {
		return strings.HasSuffix(a, "."+base)
	}
	return false
}

// domainConflicts returns the requested domains already bound by another
// site. Duplicate server_name entries silently shadow each other in nginx —
// a trap BT-style panels leave to the admin — so the panel refuses upfront.
func (m *CertManager) domainConflicts(ctx context.Context, selfID string, domains []string) []string {
	if len(domains) == 0 {
		return nil
	}
	var conflicts []string
	seen := map[string]bool{}
	for engine, dirs := range siteScanDirs {
		for _, other := range m.sites.confDirSites(ctx, dirs, engine, true) {
			if other.ID == selfID {
				continue
			}
			for _, d := range domains {
				for _, bound := range other.ServerNames {
					if bound == "_" || !domainOverlap(d, bound) {
						continue
					}
					key := d + "（已绑定于 " + strings.Join(other.ServerNames, " ") + "）"
					if !seen[key] {
						seen[key] = true
						conflicts = append(conflicts, key)
					}
				}
			}
		}
	}
	return conflicts
}

// SiteConf 应用站点高级设置（域名绑定、默认文档、整站重定向、伪静态）。
// 面板从磁盘配置重建当前状态、叠加表单修改后提交完整 spec，重渲染后所有
// 功能互不覆盖；仅面板创建的托管站点支持。
func (m *CertManager) SiteConf(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Error(w, "site id is required", 400)
		return
	}
	site, err := m.loadSite(id)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	domains, err := parseDomainList(r.FormValue("domains"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if conflicts := m.domainConflicts(ctx, site.ID, domains); len(conflicts) > 0 {
		http.Error(w, "域名已被其他站点绑定："+strings.Join(conflicts, "；"), 400)
		return
	}
	ssl, err := m.currentSSLConf(site)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	spec := specOf(site, ssl)
	spec.Domains = domains
	if site.Kind == "static" {
		spec.Index = parseIndexLines(r.FormValue("index"))
		if site.Engine == "nginx" {
			rewrite := r.FormValue("rewrite")
			if rewrite == "default" {
				rewrite = ""
			}
			if !helper.ValidRewritePreset(rewrite) {
				http.Error(w, "未知的伪静态预设", 400)
				return
			}
			spec.Rewrite = rewrite
			spec.RewriteBody = r.FormValue("rewrite_body")
		}
	}
	if target := strings.TrimSpace(r.FormValue("redirect_to")); target != "" {
		code, err := strconv.Atoi(r.FormValue("redirect_code"))
		if err != nil || code == 0 {
			code = 301
		}
		spec.Redirect = &helper.Redirect{
			Target: target, Code: code, KeepPath: r.FormValue("redirect_keep_path") == "true",
		}
	}
	req := helper.Request{
		Op: helper.OpSite, Action: "conf-apply",
		Engine: site.Engine, Site: site.SiteName, Kind: site.Kind,
		Port: strconv.Itoa(site.Port), Root: spec.Root,
		ProxyNodes: spec.Proxy.Nodes, WebSocket: spec.Proxy.WebSocket,
		Domains: spec.Domains, Index: spec.Index,
		Rewrite: spec.Rewrite, RewriteBody: spec.RewriteBody,
		SSLOn: ssl.Enabled, ForceHTTPS: ssl.ForceHTTPS,
		CertFile: ssl.CertFile, KeyFile: ssl.KeyFile,
	}
	if spec.Redirect != nil {
		req.RedirectTarget = spec.Redirect.Target
		req.RedirectCode = spec.Redirect.Code
		req.RedirectKeepPath = spec.Redirect.KeepPath
	}
	if out, routed, err := privileged(ctx, req); routed {
		if err != nil {
			commandError(w, out, err)
			return
		}
		JSON(w, map[string]string{"message": "站点设置已保存，" + site.Engine + " 配置已重载"})
		return
	}
	if err := m.rewriteManagedDirect(ctx, site, spec); err != nil {
		commandError(w, "", err)
		return
	}
	JSON(w, map[string]string{"message": "站点设置已保存，" + site.Engine + " 配置已重载"})
}

// configTest runs the engine's syntax check with the panel's own runner.
func configTest(ctx context.Context, engine string, run func(context.Context, time.Duration, string, ...string) (string, error)) (string, error) {
	if engine == "nginx" {
		return run(ctx, 30*time.Second, "nginx", "-t")
	}
	if out, err := RunCommand(ctx, "apache2ctl", "configtest"); err == nil {
		return out, nil
	}
	return run(ctx, 30*time.Second, "httpd", "-t")
}

// challengeSink implements certs.ChallengeSink against the privileged
// helper (or the filesystem directly in root mode).
type challengeSink struct {
	ctx context.Context
}

func (s challengeSink) Put(token, keyAuth string) error {
	req := helper.Request{Op: helper.OpSite, Action: "challenge-set", Token: token, Auth: keyAuth}
	if out, routed, err := privileged(s.ctx, req); routed {
		if err != nil {
			return fmt.Errorf("%w: %s", err, helper.TrimOutput(out))
		}
		return nil
	}
	if err := os.MkdirAll(helper.ChallengeDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(helper.ChallengeDir, token), []byte(keyAuth), 0o644)
}

func (s challengeSink) Clear(token string) error {
	req := helper.Request{Op: helper.OpSite, Action: "challenge-clear", Token: token}
	if out, routed, err := privileged(s.ctx, req); routed {
		if err != nil {
			return fmt.Errorf("%w: %s", err, helper.TrimOutput(out))
		}
		return nil
	}
	if token == "" {
		entries, err := os.ReadDir(helper.ChallengeDir)
		if err != nil {
			return nil
		}
		for _, e := range entries {
			if e.Type().IsRegular() {
				_ = os.Remove(filepath.Join(helper.ChallengeDir, e.Name()))
			}
		}
		return nil
	}
	return os.Remove(filepath.Join(helper.ChallengeDir, token))
}

// ---- HTTP API ----

// List reports the stored certificates.
func (m *CertManager) List(w http.ResponseWriter, r *http.Request) {
	items, err := m.Store.List()
	if err != nil {
		http.Error(w, "cannot list certificates: "+err.Error(), 500)
		return
	}
	JSON(w, struct {
		Items    []certs.Issued `json:"items"`
		StateDir string         `json:"state_dir"`
	}{items, m.Store.Dir})
}

func (m *CertManager) SSL(w http.ResponseWriter, r *http.Request) {
	op := r.FormValue("op")
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		http.Error(w, "site id is required", 400)
		return
	}
	site, err := m.loadSite(id)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	switch op {
	case "off":
		if !site.SSL {
			http.Error(w, "该站点尚未启用 SSL", 400)
			return
		}
		if err := m.applySSL(r.Context(), site, helper.SSLConf{Challenge: false}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		JSON(w, map[string]string{"message": "站点已关闭 SSL 并重载 Web 服务器"})
	case "issue":
		email := strings.TrimSpace(r.FormValue("email"))
		if email != "" && !helper.ValidEmail(email) {
			http.Error(w, "invalid registration email", 400)
			return
		}
		forceHTTPS := r.FormValue("force_https") == "true" || r.FormValue("force_https") == "on"
		staging := r.FormValue("staging") == "true" || r.FormValue("staging") == "on"
		tasks := m.TaskCenter()
		if tasks.Running("issue-cert", site.Domain) {
			http.Error(w, "a certificate is already being issued for this domain", 409)
			return
		}
		task := tasks.Start("issue-cert", site.Domain, "签发证书 "+site.Domain, func(ctx context.Context, appendOut func(string)) error {
			cctx, cancel := context.WithTimeout(ctx, 300*time.Second)
			defer cancel()
			return m.issueTask(cctx, site, email, staging, forceHTTPS, appendOut)
		})
		JSON(w, map[string]string{"message": "certificate issuance started for " + site.Domain, "task_id": task.ID})
	default:
		http.Error(w, "op must be issue or off", 400)
	}
}

// issueTask is one issuance round: prepare the challenge-serving
// configuration, run ACME, persist the certificate and switch the site to
// HTTPS. On failure the site configuration is restored to its prior state.
func (m *CertManager) issueTask(ctx context.Context, site *managedSite, email string, staging, forceHTTPS bool, appendOut func(string)) error {
	m.issueMu.Lock()
	defer m.issueMu.Unlock()

	out := func(format string, args ...any) {
		appendOut(fmt.Sprintf(format, args...))
	}
	// Renewal of an already-SSL site needs no configuration step: the
	// template from the previous issuance already serves the challenge path.
	phase1 := !site.SSL
	if phase1 {
		out("[1/4] 写入站点配置（开放 ACME 挑战路径）并重载 %s…", site.Engine)
		if err := m.applySSL(ctx, site, helper.SSLConf{Challenge: true}); err != nil {
			return fmt.Errorf("准备站点配置失败：%w", err)
		}
		defer func() {
			// First-time issuance that failed later: restore the plain site.
			if !site.SSL {
				_ = m.applySSL(context.WithoutCancel(ctx), site, helper.SSLConf{Challenge: false})
			}
		}()
	}
	out("[2/4] 正在通过 ACME 向 Let's Encrypt 申请证书（%s）…", map[bool]string{true: "测试", false: "生产"}[staging])
	chainPEM, keyPEM, err := m.issuer(func(line string) { appendOut("      " + line) }).Issue(ctx, site.Domain, email, staging, challengeSink{ctx: ctx})
	if err != nil {
		return err
	}
	meta := certs.Meta{
		Domain: site.Domain, Email: email, Staging: staging, ForceHTTPS: forceHTTPS,
		Site: certs.Site{
			Name: site.SiteName, Engine: site.Engine, Kind: site.Kind,
			Domain: site.Domain, Port: site.Port, Root: site.Root, ProxyTarget: site.ProxyPass,
		},
	}
	if err := m.Store.Save(meta, chainPEM, keyPEM); err != nil {
		return fmt.Errorf("保存证书文件失败：%w", err)
	}
	certPath, keyPath := m.Store.Paths(site.Domain)
	out("[3/4] 证书已保存：%s", certPath)
	if site.SSL && !phase1 {
		// Renewal: the configuration still points at the same files; reload
		// the engine so it picks up the new certificate.
		out("[4/4] 重载 %s 以加载新证书…", site.Engine)
		if _, err := m.sites.reloadEngine(ctx, site.Engine); err != nil {
			return fmt.Errorf("重载 %s 失败：%w", site.Engine, err)
		}
	} else {
		out("[4/4] 写入 HTTPS 站点配置并重载 %s…", site.Engine)
		if err := m.applySSL(ctx, site, helper.SSLConf{
			Enabled: true, ForceHTTPS: forceHTTPS, Challenge: true,
			CertFile: certPath, KeyFile: keyPath,
		}); err != nil {
			return fmt.Errorf("启用 HTTPS 失败：%w", err)
		}
		site.SSL = true
	}
	_ = challengeSink{ctx: context.WithoutCancel(ctx)}.Clear("")
	out("完成。站点 https://%s 已就绪（证书将于到期前 30 天自动续期）。", site.Domain)
	slog.Info("cert_issued", "domain", site.Domain, "staging", staging, "engine", site.Engine)
	return nil
}

// RenewLoop renews stored certificates that expire within 30 days. It runs
// until ctx is done; failures are logged and retried on the next tick.
func (m *CertManager) RenewLoop(ctx context.Context) {
	run := func() {
		items, err := m.Store.List()
		if err != nil {
			slog.Warn("cert_renew_scan", "error", err.Error())
			return
		}
		for _, item := range items {
			if time.Until(item.NotAfter) >= 30*24*time.Hour {
				continue
			}
			if item.Staging {
				continue // staging certificates are not auto-renewed
			}
			site, err := loadManagedSiteByID(item.Site)
			if err != nil {
				slog.Warn("cert_renew_skip", "domain", item.Domain, "error", err.Error())
				continue
			}
			// 站点已关闭 SSL（配置中不再有 443/SSLEngine）时不再续期。
			if !site.SSL {
				continue
			}
			if m.Tasks != nil && m.Tasks.Running("issue-cert", item.Domain) {
				continue
			}
			slog.Info("cert_renew_start", "domain", item.Domain)
			m.TaskCenter().Start("issue-cert", item.Domain, "续期证书 "+item.Domain, func(tctx context.Context, appendOut func(string)) error {
				cctx, cancel := context.WithTimeout(tctx, 300*time.Second)
				defer cancel()
				return m.issueTask(cctx, site, item.Email, item.Staging, item.ForceHTTPS, appendOut)
			})
		}
	}
	// Startup grace: let the web server and engine settle before scanning.
	timer := time.NewTimer(2 * time.Minute)
	defer timer.Stop()
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			run()
		case <-ticker.C:
			run()
		}
	}
}

// loadManagedSiteByID rebuilds the site parameters for renewal from the
// stored metadata, refusing when the configuration no longer matches (the
// site may have been deleted or rewritten by hand).
func loadManagedSiteByID(meta certs.Site) (*managedSite, error) {
	confPath, enabledPath := nativeConfPath(meta.Engine, meta.Name)
	if _, err := os.Stat(confPath); err != nil && enabledPath != "" {
		if link, e := os.Readlink(enabledPath); e == nil {
			confPath = link
		}
	}
	site, err := loadManagedSite(confPath)
	if err != nil {
		return nil, err
	}
	if site.Domain != meta.Domain || site.Kind != meta.Kind {
		return nil, fmt.Errorf("site configuration changed since issuance (%s != %s)", site.Domain, meta.Domain)
	}
	return site, nil
}
