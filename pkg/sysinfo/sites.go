package sysinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"lightpanel/pkg/helper"
)

// ManagedMarker marks configuration files and containers this module created,
// so destructive actions only ever touch objects the panel owns.
const ManagedMarker = "# managed by lightpanel"

var (
	siteNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	imageRefPattern  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,127}(:[a-zA-Z0-9._-]{1,64})?(@sha256:[a-f0-9]{64})?$`)
	dockerPortFactor = regexp.MustCompile(`:(\d+)->(\d+)/`)
)

func validSiteName(name string) bool { return siteNamePattern.MatchString(name) }

// validServerName accepts DNS hostnames, one leading wildcard label and the
// nginx catch-all "_". It is a syntactic gate only; everything ends up inside
// a quoted-safe config template without shell involvement.
func validServerName(name string) bool {
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

func validImageRef(ref string) bool {
	return imageRefPattern.MatchString(ref) && !strings.Contains(ref, "..")
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

// managedConfDirs bounds which configuration files the panel will ever read,
// list or delete. Everything outside is invisible to this module.
var managedConfDirs = []string{
	"/etc/nginx/sites-enabled/",
	"/etc/nginx/sites-available/",
	"/etc/nginx/conf.d/",
	"/etc/apache2/sites-enabled/",
	"/etc/apache2/sites-available/",
	"/etc/apache2/conf.d/",
	"/etc/httpd/conf.d/",
}

func isManagedConfPath(id string) bool {
	if !strings.HasSuffix(id, ".conf") || !strings.HasPrefix(id, "/") {
		return false
	}
	for _, dir := range managedConfDirs {
		if strings.HasPrefix(id, dir) && !strings.ContainsAny(id, "\\\x00") && !strings.Contains(id, "..") {
			return true
		}
	}
	return false
}

var engineUnits = map[string][]string{"nginx": {"nginx"}, "apache": {"apache2", "httpd"}}

type EngineInfo struct {
	Engine    string `json:"engine"`
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Version   string `json:"version"`
	Detail    string `json:"detail"`
}

// Site is one discovered web entry: an nginx/apache server block or a docker
// container publishing ports. ID is the configuration path (native) or the
// container name (docker) and doubles as the action target.
type Site struct {
	ID          string   `json:"id"`
	Engine      string   `json:"engine"`
	Kind        string   `json:"kind"`
	ServerNames []string `json:"server_names"`
	Ports       []int    `json:"ports"`
	Root        string   `json:"root"`
	ProxyPass   string   `json:"proxy_pass,omitempty"`
	SSL         bool     `json:"ssl"`
	State       string   `json:"state"`
	Managed     bool     `json:"managed"`
	Detail      string   `json:"detail"`
}

type SiteManager struct {
	Run        Runner
	RunTimeout func(context.Context, time.Duration, string, ...string) (string, error)
}

func NewSiteManager() *SiteManager {
	return &SiteManager{Run: RunCommand, RunTimeout: RunCommandTimeout}
}

// ---- engine detection ----

func (m *SiteManager) engineRunning(ctx context.Context, candidates []string) bool {
	for _, unit := range candidates {
		out, err := m.Run(ctx, "systemctl", "is-active", "--", unit)
		if err == nil && strings.TrimSpace(out) == "active" {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}

func (m *SiteManager) detectEnvironment(ctx context.Context) []EngineInfo {
	env := []EngineInfo{
		{Engine: "nginx", Installed: true},
		{Engine: "apache", Installed: true},
		{Engine: "docker", Installed: true},
	}
	if out, err := m.Run(ctx, "nginx", "-v"); err == nil {
		env[0].Version = firstLine(out)
	} else {
		env[0].Installed = false
		env[0].Detail = ErrUnavailable.Error()
	}
	env[0].Running = env[0].Installed && m.engineRunning(ctx, engineUnits["nginx"])

	for _, bin := range []string{"apache2ctl", "httpd"} {
		if out, err := m.Run(ctx, bin, "-v"); err == nil {
			env[1].Version = firstLine(out)
			break
		}
	}
	if env[1].Version == "" {
		env[1].Installed = false
		env[1].Detail = ErrUnavailable.Error()
	} else {
		env[1].Running = m.engineRunning(ctx, engineUnits["apache"])
	}

	if out, err := m.Run(ctx, "docker", "version", "--format", "{{.Server.Version}}"); err == nil {
		env[2].Version = "server " + firstLine(out)
		env[2].Running = true
	} else if errors.Is(err, ErrUnavailable) {
		env[2].Installed = false
		env[2].Detail = ErrUnavailable.Error()
	} else {
		env[2].Detail = "daemon unreachable: " + helper.TrimOutput(firstLine(out))
	}
	return env
}

func engineEnv(env []EngineInfo, engine string) EngineInfo {
	for _, e := range env {
		if e.Engine == engine {
			return e
		}
	}
	return EngineInfo{Engine: engine}
}

// ---- nginx config parsing ----

type nginxToken struct {
	text       string
	braceOpen  bool
	braceClose bool
	terminator bool
}

func nginxTokens(conf string) []nginxToken {
	var tokens []nginxToken
	for i := 0; i < len(conf); {
		c := conf[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '#':
			for i < len(conf) && conf[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == ';':
			tokens = append(tokens, nginxToken{text: string(c), braceOpen: c == '{', braceClose: c == '}', terminator: c == ';'})
			i++
		case c == '"' || c == '\'':
			quote := c
			i++
			start := i
			for i < len(conf) && conf[i] != quote {
				i++
			}
			tokens = append(tokens, nginxToken{text: conf[start:i]})
			if i < len(conf) {
				i++
			}
		default:
			start := i
			for i < len(conf) && !strings.ContainsRune(" \t\n\r#{};\"'", rune(conf[i])) {
				i++
			}
			tokens = append(tokens, nginxToken{text: conf[start:i]})
		}
	}
	return tokens
}

type nginxServer struct {
	ports       []int
	ssl         bool
	serverNames []string
	root        string
	proxyPass   string
}

func listenPort(arg string) (int, bool) {
	arg = strings.TrimSpace(arg)
	if arg == "" || strings.HasPrefix(arg, "unix:") {
		return 0, false
	}
	if p, err := strconv.Atoi(arg); err == nil {
		return p, true
	}
	if i := strings.LastIndexByte(arg, ':'); i >= 0 {
		if p, err := strconv.Atoi(arg[i+1:]); err == nil {
			return p, true
		}
	}
	return 0, false
}

// parseNginxServers extracts top-level server blocks. Nested blocks such as
// location are consumed as opaque groups; their directives are not scanned.
func parseNginxServers(conf string) []nginxServer {
	tokens := nginxTokens(conf)
	var servers []nginxServer
	for i := 0; i < len(tokens); i++ {
		if tokens[i].text != "server" || i+1 >= len(tokens) || !tokens[i+1].braceOpen {
			continue
		}
		depth, j := 1, i+2
		for ; j < len(tokens) && depth > 0; j++ {
			if tokens[j].braceOpen {
				depth++
			} else if tokens[j].braceClose {
				depth--
			}
		}
		body := tokens[i+2 : j-1]
		servers = append(servers, nginxServerFromBody(body))
		i = j - 1
	}
	return servers
}

func nginxServerFromBody(body []nginxToken) nginxServer {
	var s nginxServer
	// Linear scan: directives inside nested blocks (location, if) are also
	// seen, which is where proxy_pass normally lives. Block headers such as
	// "location / {" contribute no directive of interest.
	for i := 0; i < len(body); i++ {
		name := body[i].text
		if body[i].braceOpen || body[i].braceClose || body[i].terminator {
			continue
		}
		var args []string
		j := i + 1
		for ; j < len(body); j++ {
			if body[j].terminator || body[j].braceOpen || body[j].braceClose {
				break
			}
			args = append(args, body[j].text)
		}
		i = j
		switch name {
		case "listen":
			for _, arg := range args {
				if arg == "ssl" {
					s.ssl = true
					continue
				}
				if p, ok := listenPort(arg); ok {
					s.ports = append(s.ports, p)
				}
			}
		case "server_name":
			s.serverNames = append(s.serverNames, args...)
		case "root":
			if len(args) > 0 && s.root == "" {
				s.root = args[0]
			}
		case "proxy_pass":
			if len(args) > 0 && s.proxyPass == "" {
				s.proxyPass = args[0]
			}
		}
	}
	return s
}

// ---- apache config parsing ----

type apacheVHost struct {
	addr         string
	serverName   string
	documentRoot string
	proxyPass    string
	ssl          bool
}

func parseApacheVHosts(conf string) []apacheVHost {
	var vhosts []apacheVHost
	var current *apacheVHost
	for _, line := range strings.Split(conf, "\n") {
		trimmed := strings.TrimSpace(line)
		if i := strings.Index(trimmed, "#"); i >= 0 {
			trimmed = strings.TrimSpace(trimmed[:i])
		}
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "<virtualhost"):
			addr := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed[len("<VirtualHost"):]), ">"))
			vhosts = append(vhosts, apacheVHost{addr: addr})
			current = &vhosts[len(vhosts)-1]
		case strings.HasPrefix(lower, "</virtualhost"):
			current = nil
		case current != nil:
			fields := strings.Fields(trimmed)
			switch strings.ToLower(fields[0]) {
			case "servername":
				if len(fields) > 1 {
					current.serverName = fields[1]
				}
			case "documentroot":
				if len(fields) > 1 {
					current.documentRoot = fields[1]
				}
			case "sslengine":
				if len(fields) > 1 && strings.EqualFold(fields[1], "on") {
					current.ssl = true
				}
			case "proxypass":
				if len(fields) > 2 && current.proxyPass == "" {
					current.proxyPass = fields[2]
				}
			}
		}
	}
	return vhosts
}

func vhostPorts(addr string) []int {
	addr = strings.TrimPrefix(addr, "*:")
	addr = strings.TrimPrefix(addr, "_default_:")
	if p, err := strconv.Atoi(addr); err == nil {
		return []int{p}
	}
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		if p, err := strconv.Atoi(addr[i+1:]); err == nil {
			return []int{p}
		}
	}
	return nil
}

// ---- listing ----

func fileManaged(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > 512<<10 {
		return false
	}
	return bytes.Contains(data, []byte(ManagedMarker))
}

func (m *SiteManager) confDirSites(ctx context.Context, dirs []string, engine string, engineActive bool) []Site {
	var sites []Site
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			name := e.Name()
			if strings.HasSuffix(name, ".conf") && (e.Type().IsRegular() || e.Type()&fs.ModeSymlink != 0) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			path := filepath.Join(dir, name)
			data, err := os.ReadFile(path)
			if err != nil || len(data) > 512<<10 {
				continue
			}
			conf := string(data)
			managed := bytes.Contains(data, []byte(ManagedMarker))
			if engine == "nginx" {
				for _, block := range parseNginxServers(conf) {
					kind := "static"
					if block.proxyPass != "" {
						kind = "proxy"
					}
					names := block.serverNames
					if len(names) == 0 {
						names = []string{"_"}
					}
					sites = append(sites, Site{
						ID: path, Engine: engine, Kind: kind,
						ServerNames: names, Ports: block.ports, Root: block.root,
						ProxyPass: block.proxyPass, SSL: block.ssl,
						State:   map[bool]string{true: "active", false: "inactive"}[engineActive],
						Managed: managed, Detail: name,
					})
				}
			} else {
				for _, vh := range parseApacheVHosts(conf) {
					kind := "static"
					if vh.proxyPass != "" {
						kind = "proxy"
					}
					names := []string{}
					if vh.serverName != "" {
						names = append(names, vh.serverName)
					} else {
						names = append(names, "_")
					}
					sites = append(sites, Site{
						ID: path, Engine: engine, Kind: kind,
						ServerNames: names, Ports: vhostPorts(vh.addr), Root: vh.documentRoot,
						ProxyPass: vh.proxyPass, SSL: vh.ssl,
						State:   map[bool]string{true: "active", false: "inactive"}[engineActive],
						Managed: managed, Detail: name,
					})
				}
			}
		}
	}
	return sites
}

func (m *SiteManager) dockerSites(ctx context.Context) []Site {
	out, err := m.Run(ctx, "docker", "ps", "-a", "--format", "{{json .}}")
	if err != nil {
		return nil
	}
	var sites []Site
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var c struct {
			Names  string            `json:"Names"`
			Image  string            `json:"Image"`
			State  string            `json:"State"`
			Status string            `json:"Status"`
			Ports  string            `json:"Ports"`
			Labels map[string]string `json:"Labels"`
		}
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		publishes := false
		var ports []int
		for _, m := range dockerPortFactor.FindAllStringSubmatch(c.Ports, -1) {
			p, _ := strconv.Atoi(m[1])
			if validPort(p) {
				ports = append(ports, p)
				publishes = true
			}
		}
		if !publishes {
			continue
		}
		managed := c.Labels["lightpanel.site"] != "" || strings.HasPrefix(c.Names, "lightpanel-")
		sites = append(sites, Site{
			ID: c.Names, Engine: "docker", Kind: "container",
			ServerNames: []string{c.Image}, Ports: ports,
			State: c.State, Managed: managed,
			Detail: c.Status,
		})
	}
	return sites
}

func (m *SiteManager) Sites(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	env := m.detectEnvironment(ctx)
	nginxActive := engineEnv(env, "nginx").Running
	apacheActive := engineEnv(env, "apache").Running
	items := m.confDirSites(ctx, []string{"/etc/nginx/sites-enabled/", "/etc/nginx/conf.d/"}, "nginx", nginxActive)
	items = append(items, m.confDirSites(ctx, []string{"/etc/apache2/sites-enabled/", "/etc/apache2/conf.d/", "/etc/httpd/conf.d/"}, "apache", apacheActive)...)
	items = append(items, m.dockerSites(ctx)...)
	if items == nil {
		items = []Site{}
	}
	JSON(w, struct {
		Environment []EngineInfo `json:"environment"`
		Items       []Site       `json:"items"`
	}{env, items})
}

// ---- site creation ----

func dirExists(path string) bool {
	s, err := os.Stat(path)
	return err == nil && s.IsDir()
}

// nativeConfPath picks the distribution-specific location: Debian/Ubuntu use
// sites-available + a symlink in sites-enabled; RHEL-family and default
// installs use a single file in conf.d.
func nativeConfPath(engine, name string) (conf, enabled string) {
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

func validDocumentRoot(root string) bool {
	if len(root) < 2 || !strings.HasPrefix(root, "/") || strings.ContainsAny(root, "\\\x00") || root != path.Clean(root) {
		return false
	}
	for _, part := range strings.Split(root, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

const placeholderIndex = `<!doctype html>
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

func nginxConf(name, domain string, port int, root string) string {
	if domain == "" {
		domain = "_"
	}
	return ManagedMarker + " — site: " + name + `
server {
    listen ` + strconv.Itoa(port) + `;
    server_name ` + domain + `;
    root ` + root + `;
    index index.html index.htm;

    location / {
        try_files $uri $uri/ =404;
    }
}
`
}

func apacheConf(name, domain string, port int, root string) string {
	if domain == "" {
		domain = "_"
	}
	conf := ManagedMarker + " — site: " + name + "\n"
	if port != 80 && port != 443 {
		conf += "Listen " + strconv.Itoa(port) + "\n\n"
	}
	conf += `<VirtualHost *:` + strconv.Itoa(port) + `>
    ServerName ` + domain + `
    DocumentRoot ` + root + `

    <Directory ` + root + `>
        Require all granted
    </Directory>
</VirtualHost>
`
	return conf
}

// writeConfFile writes atomically: temp file in the same directory, fsync,
// rename. An existing file is never overwritten.
func writeConfFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".lightpanel-*.conf")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := tmp.WriteString(content); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (m *SiteManager) engineUnit(ctx context.Context, engine string) string {
	candidates, ok := engineUnits[engine]
	if !ok {
		return ""
	}
	args := append([]string{"list-unit-files"}, candidates...)
	args = append(args, "--no-legend", "--no-pager")
	out, _ := m.Run(ctx, "systemctl", args...)
	for _, c := range candidates {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, c+".service") {
				return c
			}
		}
	}
	return candidates[0]
}

func (m *SiteManager) reloadEngine(ctx context.Context, engine string) (string, error) {
	unit := m.engineUnit(ctx, engine)
	if unit == "" {
		return "", fmt.Errorf("engine %s has no systemd unit", engine)
	}
	if out, routed, err := privileged(ctx, helper.Request{Op: helper.OpService, Unit: unit + ".service", Action: "reload"}); routed {
		return out, err
	}
	return m.Run(ctx, "systemctl", "--no-ask-password", "reload", "--", unit+".service")
}

func (m *SiteManager) createNativeSite(ctx context.Context, engine, name, domain string, port int, root string) error {
	if root == "" {
		root = "/var/www/" + name
	}
	if !validDocumentRoot(root) {
		return fmt.Errorf("invalid site root path")
	}
	confPath, enabledPath := nativeConfPath(engine, name)
	if _, err := os.Stat(confPath); err == nil {
		return errConflict
	}
	if enabledPath != "" {
		if _, err := os.Lstat(enabledPath); err == nil {
			return errConflict
		}
	}
	// Only seed content for the default docroot; custom roots are assumed
	// prepared via the file manager.
	if root == "/var/www/"+name {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
		indexPath := filepath.Join(root, "index.html")
		if _, err := os.Stat(indexPath); errors.Is(err, fs.ErrNotExist) {
			if err := os.WriteFile(indexPath, []byte(placeholderIndex), 0o644); err != nil {
				return err
			}
		}
	}
	var conf string
	if engine == "nginx" {
		conf = nginxConf(name, domain, port, root)
	} else {
		conf = apacheConf(name, domain, port, root)
	}
	if err := writeConfFile(confPath, conf); err != nil {
		return err
	}
	if enabledPath != "" {
		if err := os.Symlink(confPath, enabledPath); err != nil {
			os.Remove(confPath)
			return err
		}
	}
	if _, err := m.reloadEngine(ctx, engine); err != nil {
		return fmt.Errorf("site created, but reloading %s failed: %w", engine, err)
	}
	return nil
}

var errConflict = errors.New("already exists")

func (m *SiteManager) createDockerSite(ctx context.Context, name, image string, port, containerPort int) error {
	out, err := m.Run(ctx, "docker", "ps", "-a", "--filter", "name=^lightpanel-"+name+"$", "--format", "{{.Names}}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return errConflict
	}
	args := []string{"run", "-d", "--name", "lightpanel-" + name, "--restart", "unless-stopped",
		"--label", "lightpanel.site=" + name, "-p", strconv.Itoa(port) + ":" + strconv.Itoa(containerPort)}
	args = append(args, image)
	out, err = m.RunTimeout(ctx, 240*time.Second, "docker", args...)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, helper.TrimOutput(out))
	}
	return nil
}

func (m *SiteManager) SiteCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.FormValue("name"))
	if !validSiteName(name) {
		http.Error(w, "site name must be 1-32 chars: lowercase letters, digits and hyphens, starting alphanumeric", 400)
		return
	}
	engine := r.FormValue("engine")
	domain := strings.TrimSpace(r.FormValue("domain"))
	if domain != "" && !validServerName(domain) {
		http.Error(w, "invalid server name / domain", 400)
		return
	}
	port, err := strconv.Atoi(r.FormValue("port"))
	if err != nil || !validPort(port) {
		http.Error(w, "port must be 1..65535", 400)
		return
	}
	env := m.detectEnvironment(ctx)
	switch engine {
	case "auto", "":
		for _, candidate := range []string{"nginx", "apache"} {
			if engineEnv(env, candidate).Installed {
				engine = candidate
				break
			}
		}
		if engine == "auto" || engine == "" {
			http.Error(w, "no native web server detected; install nginx/apache or deploy with docker", 400)
			return
		}
	case "nginx", "apache":
		if !engineEnv(env, engine).Installed {
			http.Error(w, engine+" is not installed on this server", 400)
			return
		}
	case "docker":
		if !engineEnv(env, "docker").Installed {
			http.Error(w, "docker is not installed on this server", 400)
			return
		}
	default:
		http.Error(w, "engine must be auto, nginx, apache or docker", 400)
		return
	}
	switch engine {
	case "nginx", "apache":
		root := strings.TrimSpace(r.FormValue("root"))
		if err := m.createNativeSite(ctx, engine, name, domain, port, root); err != nil {
			if errors.Is(err, errConflict) {
				http.Error(w, "a site with this name already exists", 409)
				return
			}
			commandError(w, "", err)
			return
		}
		JSON(w, map[string]string{"message": "site created and " + engine + " reloaded", "root": defaultRoot(root, name), "engine": engine})
	case "docker":
		image := strings.TrimSpace(r.FormValue("image"))
		containerPort, err := strconv.Atoi(r.FormValue("container_port"))
		if err != nil || containerPort == 0 {
			containerPort = 80
		}
		if !validPort(containerPort) || !validImageRef(image) {
			http.Error(w, "invalid image reference or container port (1..65535)", 400)
			return
		}
		if err := m.createDockerSite(ctx, name, image, port, containerPort); err != nil {
			if errors.Is(err, errConflict) {
				http.Error(w, "a site with this name already exists", 409)
				return
			}
			commandError(w, "", err)
			return
		}
		JSON(w, map[string]string{"message": "container started", "container": "lightpanel-" + name, "engine": "docker"})
	}
}

func defaultRoot(root, name string) string {
	if root == "" {
		return "/var/www/" + name
	}
	return root
}

// ---- site actions ----

func (m *SiteManager) dockerManagedNames(ctx context.Context) (map[string]bool, error) {
	out, err := m.Run(ctx, "docker", "ps", "-a", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] != '{' {
			continue
		}
		var c struct {
			Names  string            `json:"Names"`
			Labels map[string]string `json:"Labels"`
		}
		if json.Unmarshal([]byte(line), &c) == nil &&
			(c.Labels["lightpanel.site"] != "" || strings.HasPrefix(c.Names, "lightpanel-")) {
			names[c.Names] = true
		}
	}
	return names, nil
}

func (m *SiteManager) deleteNativeSite(id string) error {
	if !isManagedConfPath(id) {
		return fmt.Errorf("configuration path is outside the managed directories")
	}
	target := id
	if fi, err := os.Lstat(id); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if link, err := os.Readlink(id); err == nil {
			target = link
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if len(data) > 512<<10 || !bytes.Contains(data, []byte(ManagedMarker)) {
		return fmt.Errorf("refusing to delete: configuration was not created by lightpanel")
	}
	if err := os.Remove(id); err != nil {
		return err
	}
	if target != id {
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (m *SiteManager) SiteAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := strings.TrimSpace(r.FormValue("id"))
	op := r.FormValue("op")
	engine := r.FormValue("engine")
	if id == "" {
		http.Error(w, "site id is required", 400)
		return
	}
	switch op {
	case "reload":
		if engine != "nginx" && engine != "apache" {
			http.Error(w, "reload requires a native engine", 400)
			return
		}
		if out, err := m.reloadEngine(ctx, engine); err != nil {
			commandError(w, out, err)
			return
		}
		JSON(w, map[string]string{"message": engine + " configuration reloaded"})
	case "start", "stop", "delete":
		names, err := m.dockerManagedNames(ctx)
		if err != nil {
			commandError(w, "", err)
			return
		}
		if !names[id] {
			http.Error(w, "refusing to act on a container that was not created by lightpanel", 403)
			return
		}
		switch op {
		case "start":
			if out, err := m.Run(ctx, "docker", "start", id); err != nil {
				commandError(w, out, err)
				return
			}
			JSON(w, map[string]string{"message": "container started"})
		case "stop":
			if out, err := m.RunTimeout(ctx, 40*time.Second, "docker", "stop", id); err != nil {
				commandError(w, out, err)
				return
			}
			JSON(w, map[string]string{"message": "container stopped"})
		case "delete":
			if out, err := m.RunTimeout(ctx, 60*time.Second, "docker", "rm", "-f", id); err != nil {
				commandError(w, out, err)
				return
			}
			JSON(w, map[string]string{"message": "container removed"})
		}
	default:
		http.Error(w, "op must be start, stop, delete or reload", 400)
	}
}
