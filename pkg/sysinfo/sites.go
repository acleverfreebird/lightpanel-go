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

	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"lightpanel/pkg/helper"
)

// Site validation, configuration templates and path rules live in
// pkg/helper/sites.go and are shared with the helper process, so both ends
// accept exactly the same inputs. The helpers below keep the historical
// unexported call sites readable.
var dockerPortFactor = regexp.MustCompile(`:(\d+)->(\d+)/`)

func validSiteName(name string) bool              { return helper.ValidSiteName(name) }
func validServerName(name string) bool            { return helper.ValidServerName(name) }
func validImageRef(ref string) bool               { return helper.ValidImageRef(ref) }
func validPort(p int) bool                        { return helper.ValidPort(p) }
func isManagedConfPath(id string) bool            { return helper.IsManagedConfPath(id) }
func validDocumentRoot(root string) bool          { return helper.ValidDocumentRoot(root) }
func nativeConfPath(e, n string) (string, string) { return helper.NativeConfPath(e, n) }

var engineUnits = helper.EngineUnits

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
	// certbot gates Let's Encrypt issuance; without it the certificate panel
	// reports 501 and the per-site action is hidden by the frontend.
	if out, err := m.Run(ctx, "certbot", "--version"); err == nil {
		env = append(env, EngineInfo{Engine: "certbot", Installed: true, Version: firstLine(out)})
	} else {
		env = append(env, EngineInfo{Engine: "certbot", Installed: false, Detail: ErrUnavailable.Error()})
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
	return bytes.Contains(data, []byte(helper.ManagedMarker))
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
			managed := bytes.Contains(data, []byte(helper.ManagedMarker))
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
	// Site reloads belong to the allow_sites grant; the helper resolves the
	// engine's unit itself, so no per-unit service ACL entry is needed.
	if out, routed, err := privileged(ctx, helper.Request{Op: helper.OpSite, Action: "reload", Engine: engine}); routed {
		return out, err
	}
	return m.Run(ctx, "systemctl", "--no-ask-password", "reload", "--", unit+".service")
}

// createNativeSite writes the site configuration either through the helper
// (least-privilege mode: the helper re-validates and re-renders everything)
// or directly with the panel's own privileges (root mode).
func (m *SiteManager) createNativeSite(ctx context.Context, engine, kind, name, domain string, port int, root, proxyTarget string) error {
	if kind == "" {
		kind = "static"
	}
	if kind != "static" && kind != "proxy" {
		return fmt.Errorf("kind must be static or proxy")
	}
	if out, routed, err := privileged(ctx, helper.Request{
		Op: helper.OpSite, Action: "create", Engine: engine, Site: name, Kind: kind,
		Domain: domain, Port: strconv.Itoa(port), Root: root, ProxyTarget: proxyTarget,
	}); routed {
		if err != nil {
			if strings.Contains(err.Error(), "already exists") {
				return errConflict
			}
			return fmt.Errorf("%w\n%s", err, helper.TrimOutput(out))
		}
		return nil
	}
	if kind == "proxy" {
		if !helper.ValidProxyTarget(proxyTarget) {
			return fmt.Errorf("invalid proxy target")
		}
		root = ""
	} else {
		if root == "" {
			root = "/var/www/" + name
		}
		if !validDocumentRoot(root) {
			return fmt.Errorf("invalid site root path")
		}
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
	if kind == "static" && root == "/var/www/"+name {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
		indexPath := filepath.Join(root, "index.html")
		if _, err := os.Stat(indexPath); errors.Is(err, fs.ErrNotExist) {
			if err := os.WriteFile(indexPath, []byte(helper.PlaceholderIndex), 0o644); err != nil {
				return err
			}
		}
	}
	conf, err := helper.SiteConf(engine, kind, name, domain, port, root, proxyTarget)
	if err != nil {
		return err
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
		kind := r.FormValue("mode")
		if kind == "" {
			kind = "static"
		}
		proxyTarget := strings.TrimSpace(r.FormValue("proxy_target"))
		if kind == "proxy" {
			if !helper.ValidProxyTarget(proxyTarget) {
				http.Error(w, "proxy target must be http(s)://host[:port][/path]", 400)
				return
			}
		} else if kind != "static" {
			http.Error(w, "mode must be static or proxy", 400)
			return
		}
		root := strings.TrimSpace(r.FormValue("root"))
		if err := m.createNativeSite(ctx, engine, kind, name, domain, port, root, proxyTarget); err != nil {
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
	if len(data) > 512<<10 || !bytes.Contains(data, []byte(helper.ManagedMarker)) {
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
	case "delete":
		if engine == "nginx" || engine == "apache" {
			// In least-privilege mode the helper re-checks the managed path
			// and marker; a root panel applies the same checks locally.
			if out, routed, err := privileged(ctx, helper.Request{Op: helper.OpSite, Action: "delete", Path: id}); routed {
				if err != nil {
					commandError(w, out, err)
					return
				}
			} else if err := m.deleteNativeSite(id); err != nil {
				commandError(w, "", err)
				return
			}
			if out, err := m.reloadEngine(ctx, engine); err != nil {
				commandError(w, out, err)
				return
			}
			JSON(w, map[string]string{"message": "site deleted and " + engine + " reloaded"})
			return
		}
		names, err := m.dockerManagedNames(ctx)
		if err != nil {
			commandError(w, "", err)
			return
		}
		if !names[id] {
			http.Error(w, "refusing to act on a container that was not created by lightpanel", 403)
			return
		}
		if out, err := m.RunTimeout(ctx, 60*time.Second, "docker", "rm", "-f", id); err != nil {
			commandError(w, out, err)
			return
		}
		JSON(w, map[string]string{"message": "container removed"})
	case "start", "stop":
		names, err := m.dockerManagedNames(ctx)
		if err != nil {
			commandError(w, "", err)
			return
		}
		if !names[id] {
			http.Error(w, "refusing to act on a container that was not created by lightpanel", 403)
			return
		}
		if op == "start" {
			if out, err := m.Run(ctx, "docker", "start", id); err != nil {
				commandError(w, out, err)
				return
			}
			JSON(w, map[string]string{"message": "container started"})
			return
		}
		if out, err := m.RunTimeout(ctx, 40*time.Second, "docker", "stop", id); err != nil {
			commandError(w, out, err)
			return
		}
		JSON(w, map[string]string{"message": "container stopped"})
	default:
		http.Error(w, "op must be start, stop, delete or reload", 400)
	}
}

// ---- HTTPS certificates (Let's Encrypt via certbot) ----

// Certificates reports certbot availability and the local certificate list.
// certbot output is shown verbatim (failures included) so the admin can see
// why a listing is empty or stale.
func (m *SiteManager) Certificates(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out, routed, err := privileged(ctx, helper.Request{Op: helper.OpSite, Action: "cert-status"})
	if !routed {
		out, err = m.Run(ctx, "certbot", "certificates")
	}
	installed := !errors.Is(err, ErrUnavailable)
	if !installed {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(501)
		_ = json.NewEncoder(w).Encode(struct {
			Installed bool   `json:"installed"`
			Output    string `json:"output"`
			Error     string `json:"error,omitempty"`
		}{false, out, helper.TrimOutput(err.Error())})
		return
	}
	errText := ""
	if err != nil {
		errText = helper.TrimOutput(err.Error())
	}
	JSON(w, struct {
		Installed bool   `json:"installed"`
		Output    string `json:"output"`
		Error     string `json:"error,omitempty"`
	}{installed, out, errText})
}

// IssueCert requests a Let's Encrypt certificate for one domain with certbot.
// The engine installer plugin rewrites the site's server block for HTTPS.
func (m *SiteManager) IssueCert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	domain := strings.TrimSpace(r.FormValue("domain"))
	email := strings.TrimSpace(r.FormValue("email"))
	engine := r.FormValue("engine")
	if !validServerName(domain) || domain == "_" || strings.HasPrefix(domain, "*.") {
		http.Error(w, "certificate issuance needs one concrete domain (wildcards require DNS-01)", 400)
		return
	}
	if !helper.ValidEmail(email) {
		http.Error(w, "a valid registration email is required", 400)
		return
	}
	if engine != "nginx" && engine != "apache" {
		http.Error(w, "engine must be nginx or apache", 400)
		return
	}
	// The panel forwards with a long deadline so the ACME round-trip survives.
	cctx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()
	args := []string{"-n", "--agree-tos", "-m", email, "-d", domain, "--" + engine}
	out, routed, err := privileged(cctx, helper.Request{Op: helper.OpSite, Action: "issue-cert", Domain: domain, Email: email, Engine: engine})
	if !routed {
		out, err = m.RunTimeout(cctx, 280*time.Second, "certbot", args...)
	}
	if err != nil {
		commandError(w, out, err)
		return
	}
	JSON(w, map[string]string{"message": "certificate issued for " + domain, "output": helper.TrimOutput(out)})
}
