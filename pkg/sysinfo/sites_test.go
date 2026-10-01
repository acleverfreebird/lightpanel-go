package sysinfo

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"lightpanel/pkg/certs"
	"lightpanel/pkg/helper"
)

func TestParseNginxServers(t *testing.T) {
	conf := `
user www-data;
# commented server { should be ignored
events { worker_connections 768; }
http {
    include /etc/nginx/mime.types;
    server {
        listen 80 default_server;
        listen [::]:80;
        server_name example.com www.example.com;
        root /var/www/example;
        location / { try_files $uri $uri/ =404; }
    }
    server {
        listen 1.2.3.4:8443 ssl http2;
        server_name app.example.com;
        location /api/ {
            proxy_pass http://127.0.0.1:9000/;
        }
    }
}
`
	servers := parseNginxServers(conf)
	if len(servers) != 2 {
		t.Fatalf("want 2 server blocks, got %d: %+v", len(servers), servers)
	}
	first, second := servers[0], servers[1]
	if !reflect.DeepEqual(first.ports, []int{80, 80}) {
		t.Fatalf("wrong ports: %v", first.ports)
	}
	if first.root != "/var/www/example" || first.ssl || first.proxyPass != "" {
		t.Fatalf("wrong first server: %+v", first)
	}
	if !reflect.DeepEqual(first.serverNames, []string{"example.com", "www.example.com"}) {
		t.Fatalf("wrong names: %v", first.serverNames)
	}
	if !reflect.DeepEqual(second.ports, []int{8443}) || !second.ssl {
		t.Fatalf("wrong second server: %+v", second)
	}
	if second.proxyPass != "http://127.0.0.1:9000/" {
		t.Fatalf("proxy_pass not captured: %+v", second)
	}
}

func TestParseNginxServersQuotedAndNested(t *testing.T) {
	conf := `server {
    listen 443 ssl;
    server_name "weird name.example";
    root /srv/web;
    location ~ \.php$ {
        include snippets/fastcgi-php.conf;
        fastcgi_pass unix:/run/php/php-fpm.sock;
    }
    location /api/ { proxy_pass http://upstream:8080; }
}`
	servers := parseNginxServers(conf)
	if len(servers) != 1 {
		t.Fatalf("want 1 server, got %d", len(servers))
	}
	s := servers[0]
	if s.serverNames[0] != "weird name.example" || s.root != "/srv/web" || !s.ssl {
		t.Fatalf("wrong parse: %+v", s)
	}
	// proxy_pass typically lives inside a location block; the linear scan
	// captures it so reverse-proxy sites are detected correctly.
	if s.proxyPass != "http://upstream:8080" {
		t.Fatalf("nested proxy_pass missed: %+v", s)
	}
}

func TestParseApacheVHosts(t *testing.T) {
	conf := `# comment <VirtualHost *:99>
Listen 8080
<VirtualHost *:80>
    ServerName a.example.com
    DocumentRoot /var/www/a
</VirtualHost>
<VirtualHost *:443>
    ServerName b.example.com
    DocumentRoot "/var/www/b"
    SSLEngine on
    ProxyPass /app http://127.0.0.1:3000/
</VirtualHost>
`
	vhosts := parseApacheVHosts(conf)
	if len(vhosts) != 2 {
		t.Fatalf("want 2 vhosts, got %d: %+v", len(vhosts), vhosts)
	}
	if vhosts[0].serverName != "a.example.com" || vhosts[0].documentRoot != "/var/www/a" || vhosts[0].ssl {
		t.Fatalf("wrong first vhost: %+v", vhosts[0])
	}
	if !reflect.DeepEqual(vhostPorts(vhosts[0].addr), []int{80}) {
		t.Fatalf("wrong port: %v", vhosts[0].addr)
	}
	if !vhosts[1].ssl || vhosts[1].proxyPass != "http://127.0.0.1:3000/" {
		t.Fatalf("wrong second vhost: %+v", vhosts[1])
	}
	if !reflect.DeepEqual(vhostPorts("_default_:8443"), []int{8443}) || !reflect.DeepEqual(vhostPorts("10.0.0.1:8080"), []int{8080}) {
		t.Fatalf("vhostPorts variants failed")
	}
}

func TestSiteValidation(t *testing.T) {
	valid := []string{"blog", "a1", "my-site-2"}
	for _, name := range valid {
		if !validSiteName(name) {
			t.Errorf("validSiteName(%q) rejected", name)
		}
	}
	invalid := []string{"", "-a", "UPPER", "a b", "site.name", "很棒", strings.Repeat("a", 40)}
	for _, name := range invalid {
		if validSiteName(name) {
			t.Errorf("validSiteName(%q) accepted", name)
		}
	}
	if !validServerName("_") || !validServerName("*.example.com") || !validServerName("a-b.example.co.uk") {
		t.Error("valid domains rejected")
	}
	for _, d := range []string{"", "-a.b", "a-.b", "bad..name", "x y.z", "*.com-"} {
		if validServerName(d) {
			t.Errorf("validServerName(%q) accepted", d)
		}
	}
	if !validImageRef("nginx") || !validImageRef("nginx:1.27") || !validImageRef("ghcr.io/owner/app:v1.0") {
		t.Error("valid image refs rejected")
	}
	for _, img := range []string{"", "nginx; rm -rf /", "nginx -v", "-bad", "a..b", "nginx:bad tag"} {
		if validImageRef(img) {
			t.Errorf("validImageRef(%q) accepted", img)
		}
	}
	if !validDocumentRoot("/var/www/blog") || validDocumentRoot("/") || validDocumentRoot("/var/../etc") || validDocumentRoot("var/www") || validDocumentRoot("") {
		t.Error("validDocumentRoot boundary failed")
	}
	if !isManagedConfPath("/etc/nginx/sites-enabled/blog.conf") || !isManagedConfPath("/etc/httpd/conf.d/blog.conf") {
		t.Error("managed conf path rejected")
	}
	for _, p := range []string{"/etc/nginx/nginx.conf.d/../evil.conf", "/etc/other/x.conf", "etc/nginx/a.conf", "/etc/nginx/conf.d/x", "/var/www/x.conf"} {
		if isManagedConfPath(p) {
			t.Errorf("isManagedConfPath(%q) accepted", p)
		}
	}
}

func siteEnvRunner(t *testing.T, calls *[]string) Runner {
	t.Helper()
	return func(_ context.Context, command string, args ...string) (string, error) {
		*calls = append(*calls, command+" "+strings.Join(args, " "))
		key := command
		if len(args) > 0 && (args[0] == "-v" || args[0] == "version" || args[0] == "is-active") {
			key = command + " " + args[0]
		}
		switch key {
		case "nginx -v":
			return "nginx version: nginx/1.27.2\n", nil
		case "apache2ctl -v":
			return "Server version: Apache/2.4.62 (Debian)\n", nil
		case "docker version":
			return "27.3.1\n", nil
		case "systemctl is-active":
			if args[len(args)-1] == "apache2" {
				return "active\n", nil
			}
			return "inactive\n", errors.New("exit 3")
		default:
			return "", nil
		}
	}
}

func TestDetectEnvironment(t *testing.T) {
	var calls []string
	m := &SiteManager{Run: siteEnvRunner(t, &calls), RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) { return "", nil }}
	env := m.detectEnvironment(context.Background())
	byName := map[string]EngineInfo{}
	for _, e := range env {
		byName[e.Engine] = e
	}
	if !byName["nginx"].Installed || byName["nginx"].Version != "nginx version: nginx/1.27.2" || byName["nginx"].Running {
		t.Fatalf("nginx detection wrong: %+v", byName["nginx"])
	}
	if !byName["apache"].Installed || !byName["apache"].Running || byName["apache"].Version == "" {
		t.Fatalf("apache detection wrong: %+v", byName["apache"])
	}
	if !byName["docker"].Installed || !byName["docker"].Running {
		t.Fatalf("docker detection wrong: %+v", byName["docker"])
	}
}

func TestDockerSitesParsing(t *testing.T) {
	lines := strings.Join([]string{
		`{"Names":"lightpanel-blog","Image":"nginx:1.27","State":"running","Status":"Up 2 minutes","Ports":"0.0.0.0:8080->80/tcp, :::8080->80/tcp","Labels":{"lightpanel.site":"blog"}}`,
		`{"Names":"pg","Image":"postgres:16","State":"running","Status":"Up 3 days","Ports":"127.0.0.1:5432->5432/tcp","Labels":{}}`,
		`{"Names":"internal","Image":"redis:7","State":"exited","Status":"Exited (0)","Ports":"","Labels":{}}`,
		"not json",
		"",
	}, "\n")
	m := &SiteManager{Run: func(context.Context, string, ...string) (string, error) { return lines, nil }}
	sites := m.dockerSites(context.Background())
	if len(sites) != 2 {
		t.Fatalf("want 2 sites with published ports, got %d: %+v", len(sites), sites)
	}
	if sites[0].ID != "lightpanel-blog" || !sites[0].Managed || !reflect.DeepEqual(sites[0].Ports, []int{8080, 8080}) || sites[0].State != "running" {
		t.Fatalf("managed container parsed wrong: %+v", sites[0])
	}
	if sites[1].ID != "pg" || sites[1].Managed {
		t.Fatalf("unmanaged container parsed wrong: %+v", sites[1])
	}
}

func TestCreateDockerSiteCommand(t *testing.T) {
	var got [][]string
	m := &SiteManager{
		Run: func(_ context.Context, _ string, args ...string) (string, error) {
			return "", nil // docker ps name check: no conflict
		},
		RunTimeout: func(_ context.Context, timeout time.Duration, _ string, args ...string) (string, error) {
			got = append(got, args)
			if timeout != 240*time.Second {
				t.Errorf("expected 240s docker run timeout, got %v", timeout)
			}
			return "abc123\n", nil
		},
	}
	if err := m.createDockerSite(context.Background(), "blog", "nginx:1.27", 8080, 80, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "-d", "--name", "lightpanel-blog", "--restart", "unless-stopped",
		"--label", "lightpanel.site=blog", "-p", "8080:80", "nginx:1.27"}
	if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
		t.Fatalf("wrong docker args: %v", got)
	}
	// conflict: existing container with the managed name
	m.Run = func(_ context.Context, _ string, args ...string) (string, error) {
		if args[0] == "ps" {
			return "lightpanel-blog\n", nil
		}
		return "", nil
	}
	if err := m.createDockerSite(context.Background(), "blog", "nginx", 80, 80, nil); !errors.Is(err, errConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestSiteActionGuardrails(t *testing.T) {
	request := func(m *SiteManager, form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/sites/action", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		m.SiteAction(w, r)
		return w
	}
	// an unmanaged container must never be acted on
	var ran []string
	m := &SiteManager{
		Run: func(_ context.Context, _ string, args ...string) (string, error) {
			ran = append(ran, strings.Join(args, " "))
			if args[0] == "ps" {
				return `{"Names":"victim","Image":"nginx","State":"running","Status":"Up","Ports":"0.0.0.0:80->80/tcp","Labels":{}}` + "\n", nil
			}
			return "", nil
		},
		RunTimeout: func(_ context.Context, _ time.Duration, _ string, args ...string) (string, error) {
			ran = append(ran, "timeout:"+strings.Join(args, " "))
			return "", nil
		},
	}
	if w := request(m, url.Values{"id": {"victim"}, "op": {"stop"}}); w.Code != 403 {
		t.Fatalf("unmanaged container must be refused, got %d %s", w.Code, w.Body.String())
	}
	if len(ran) != 1 { // only the docker ps lookup ran
		t.Fatalf("unexpected commands: %v", ran)
	}
	if w := request(m, url.Values{"id": {"evil"}, "op": {"stop"}}); w.Code != 403 {
		t.Fatalf("unknown container must be refused, got %d", w.Code)
	}
	// managed container: stop is executed with the documented grace period
	m.Run = func(_ context.Context, _ string, args ...string) (string, error) {
		if args[0] == "ps" {
			return `{"Names":"lightpanel-blog","Image":"nginx","State":"running","Status":"Up","Ports":"0.0.0.0:80->80/tcp","Labels":{"lightpanel.site":"blog"}}` + "\n", nil
		}
		return "", nil
	}
	if w := request(m, url.Values{"id": {"lightpanel-blog"}, "op": {"stop"}}); w.Code != 200 {
		t.Fatalf("managed stop failed: %d %s", w.Code, w.Body.String())
	}
	if w := request(m, url.Values{"id": {"lightpanel-blog"}, "op": {"pause"}}); w.Code != 400 {
		t.Fatalf("unknown op must be rejected, got %d", w.Code)
	}
}

func TestSiteCreateValidation(t *testing.T) {
	m := &SiteManager{
		Run: siteEnvRunner(t, &[]string{}),
		RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) {
			return "", nil
		},
	}
	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/sites/create", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		m.SiteCreate(w, r)
		return w
	}
	if w := post(url.Values{"name": {"Bad Name"}, "engine": {"docker"}, "port": {"8080"}, "image": {"nginx"}}); w.Code != 400 {
		t.Fatalf("invalid name accepted: %d", w.Code)
	}
	if w := post(url.Values{"name": {"blog"}, "engine": {"docker"}, "port": {"0"}, "image": {"nginx"}}); w.Code != 400 {
		t.Fatalf("invalid port accepted: %d", w.Code)
	}
	if w := post(url.Values{"name": {"blog"}, "engine": {"docker"}, "port": {"80"}, "image": {"nginx; poweroff"}}); w.Code != 400 {
		t.Fatalf("unsafe image accepted: %d", w.Code)
	}
	if w := post(url.Values{"name": {"blog"}, "engine": {"docker"}, "port": {"80"}, "image": {"nginx"}, "container_port": {"99999"}}); w.Code != 400 {
		t.Fatalf("invalid container_port accepted: %d", w.Code)
	}
}

func TestSiteTemplates(t *testing.T) {
	nginx, err := helper.SiteConf("nginx", "static", "blog", "blog.example.com", 8080, "/var/www/blog", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(nginx, helper.ManagedMarker) || !strings.Contains(nginx, "listen 8080;") ||
		!strings.Contains(nginx, "server_name blog.example.com;") || !strings.Contains(nginx, "root /var/www/blog;") {
		t.Fatalf("nginx template wrong:\n%s", nginx)
	}
	apache, err := helper.SiteConf("apache", "static", "blog", "", 8080, "/var/www/blog", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(apache, "ServerName _") || !strings.Contains(apache, "Listen 8080") {
		t.Fatalf("apache template wrong:\n%s", apache)
	}
	apache80, _ := helper.SiteConf("apache", "static", "blog", "a.example.com", 80, "/var/www/blog", "")
	if strings.Contains(apache80, "Listen ") {
		t.Fatalf("port 80 must not emit a Listen directive:\n%s", apache80)
	}
}

func TestProxySiteTemplates(t *testing.T) {
	nginx, err := helper.SiteConf("nginx", "proxy", "app", "app.example.com", 80, "", "http://127.0.0.1:3000")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nginx, "proxy_pass http://127.0.0.1:3000;") ||
		!strings.Contains(nginx, "proxy_set_header Host $host;") || strings.Contains(nginx, "root ") {
		t.Fatalf("nginx proxy template wrong:\n%s", nginx)
	}
	apache, err := helper.SiteConf("apache", "proxy", "app", "app.example.com", 80, "", "http://127.0.0.1:3000")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(apache, "ProxyPass / http://127.0.0.1:3000") || !strings.Contains(apache, "ProxyPassReverse /") {
		t.Fatalf("apache proxy template wrong:\n%s", apache)
	}
	for _, bad := range []struct{ target string }{{"ftp://x"}, {"http://x/y?z=1"}, {"http://"}, {"http://x:99999"}, {"javascript:alert(1)"}} {
		if _, err := helper.SiteConf("nginx", "proxy", "app", "app.example.com", 80, "", bad.target); err == nil {
			t.Errorf("proxy target %q accepted", bad.target)
		}
	}
	if _, err := helper.SiteConf("nginx", "proxy", "app", "app.example.com", 80, "/var/www/app", ""); err == nil {
		t.Error("proxy site without target accepted")
	}
	if _, err := helper.SiteConf("nginx", "static", "app", "app.example.com", 80, "", ""); err == nil {
		t.Error("static site without root accepted")
	}
}

func TestCreateNativeSiteRoutesThroughHelper(t *testing.T) {
	previous := PrivilegedCall
	t.Cleanup(func() { PrivilegedCall = previous })
	var got helper.Request
	PrivilegedCall = func(_ context.Context, req helper.Request) (string, error) {
		got = req
		return "", nil
	}
	m := &SiteManager{Run: func(context.Context, string, ...string) (string, error) { return "", nil },
		RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) { return "", nil }}
	if err := m.createNativeSite(context.Background(), "nginx", "proxy", "app", "app.example.com", 8080, "", "http://127.0.0.1:3000", nil); err != nil {
		t.Fatalf("helper-routed create failed: %v", err)
	}
	if got.Op != helper.OpSite || got.Action != "create" || got.Kind != "proxy" || got.Site != "app" ||
		got.Port != "8080" || got.ProxyTarget != "http://127.0.0.1:3000" {
		t.Fatalf("wrong helper request: %+v", got)
	}
	// helper-side conflict surfaces as errConflict so the API answers 409
	PrivilegedCall = func(_ context.Context, _ helper.Request) (string, error) {
		return "", errors.New("helper operation failed: a site with this name already exists")
	}
	if err := m.createNativeSite(context.Background(), "nginx", "static", "blog", "", 80, "", "", nil); !errors.Is(err, errConflict) {
		t.Fatalf("conflict not mapped: %v", err)
	}
}

func TestCreateNativeSiteSurfacesHelperOutput(t *testing.T) {
	previous := PrivilegedCall
	t.Cleanup(func() { PrivilegedCall = previous })
	PrivilegedCall = func(_ context.Context, req helper.Request) (string, error) {
		if req.Action != "create" {
			return "", errors.New("unexpected action " + req.Action)
		}
		// helper 创建站点时会停用发行版默认站点，说明文字随 Output 返回
		return "已停用发行版默认站点（/etc/nginx/sites-enabled/default）\n", nil
	}
	m := &SiteManager{Run: func(context.Context, string, ...string) (string, error) { return "", nil },
		RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) { return "", nil }}
	var notes []string
	if err := m.createNativeSite(context.Background(), "nginx", "static", "blog", "", 80, "", "",
		func(s string) { notes = append(notes, s) }); err != nil {
		t.Fatalf("helper-routed create failed: %v", err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "已停用") {
		t.Fatalf("helper note not relayed to task output: %v", notes)
	}
}

func TestParseProxyUpstreamsAndWebSocket(t *testing.T) {
	conf, err := helper.SiteConfEx("nginx", "proxy", "app", "app.example.com", 80, "", helper.ProxyConf{
		Nodes: []helper.ProxyNode{
			{Target: "http://10.0.0.1:8080", Weight: 3},
			{Target: "http://10.0.0.2:8080", Backup: true},
		},
		WebSocket: true,
	}, helper.SSLConf{})
	if err != nil {
		t.Fatal(err)
	}
	groups := parseNginxUpstreams(conf)
	if len(groups) != 1 || groups[0].name != "lightpanel-app" {
		t.Fatalf("upstream group parsed wrong: %+v", groups)
	}
	if len(groups[0].servers) != 2 || groups[0].servers[0].Weight != 3 || !groups[0].servers[1].Backup {
		t.Fatalf("upstream members parsed wrong: %+v", groups[0].servers)
	}
	if up := upstreamFor(groups, "http://lightpanel-app"); up == nil || len(up.servers) != 2 {
		t.Fatalf("proxy_pass not resolved to its upstream: %+v", up)
	}
	if up := upstreamFor(groups, "http://other"); up != nil {
		t.Fatalf("unrelated proxy_pass must not resolve: %+v", up)
	}
	servers := parseNginxServers(conf)
	if len(servers) != 1 || servers[0].proxyPass != "http://lightpanel-app" || !servers[0].proxyWS {
		t.Fatalf("server block parsed wrong: %+v", servers)
	}
	if len(servers[0].ports) != 1 || servers[0].ports[0] != 80 {
		t.Fatalf("upstream server entries must not leak into listen parsing: %+v", servers[0].ports)
	}
}

func TestParseApacheBalancer(t *testing.T) {
	conf := `# managed by lightpanel — site: app
<Proxy balancer://lightpanel-app>
    BalancerMember http://10.0.0.1:8080 loadfactor=3
    BalancerMember http://10.0.0.2:8080 status=+H
</Proxy>

<VirtualHost *:80>
    ServerName app.example.com

    ProxyPreserveHost On
    ProxyPass / balancer://lightpanel-app/
    ProxyPassReverse / balancer://lightpanel-app/
</VirtualHost>
`
	vhosts := parseApacheVHosts(conf)
	if len(vhosts) != 1 {
		t.Fatalf("want 1 vhost, got %d: %+v", len(vhosts), vhosts)
	}
	v := vhosts[0]
	if v.proxyPass != "balancer://lightpanel-app/" || len(v.proxyNodes) != 2 {
		t.Fatalf("balancer vhost parsed wrong: %+v", v)
	}
	if v.proxyNodes[0].Weight != 3 || !v.proxyNodes[1].Backup {
		t.Fatalf("balancer members parsed wrong: %+v", v.proxyNodes)
	}
}

func TestConfDirSitesProxyNodes(t *testing.T) {
	dir := t.TempDir()
	conf, err := helper.SiteConfEx("nginx", "proxy", "app", "app.example.com", 80, "", helper.ProxyConf{
		Nodes: []helper.ProxyNode{
			{Target: "http://10.0.0.1:8080", Weight: 2},
			{Target: "http://10.0.0.2:8080", Backup: true},
		},
		WebSocket: true,
	}, helper.SSLConf{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &SiteManager{Run: func(context.Context, string, ...string) (string, error) { return "", nil }}
	sites := m.confDirSites(context.Background(), []string{dir}, "nginx", true)
	if len(sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(sites))
	}
	s := sites[0]
	if s.Kind != "proxy" || len(s.ProxyNodes) != 2 || s.ProxyNodes[0].Weight != 2 || !s.ProxyNodes[1].Backup || !s.WebSocket {
		t.Fatalf("site proxy topology parsed wrong: %+v", s)
	}
	// a plain single-target proxy site round-trips as one node
	single, err := helper.SiteConf("nginx", "proxy", "api", "api.example.com", 80, "", "http://127.0.0.1:3000")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api.conf"), []byte(single), 0o644); err != nil {
		t.Fatal(err)
	}
	sites = m.confDirSites(context.Background(), []string{dir}, "nginx", true)
	if len(sites) != 2 {
		t.Fatalf("want 2 sites, got %d", len(sites))
	}
	for _, s := range sites {
		if s.Detail == "api.conf" && (len(s.ProxyNodes) != 1 || s.ProxyNodes[0].Target != "http://127.0.0.1:3000" || s.WebSocket) {
			t.Fatalf("single-target site parsed wrong: %+v", s)
		}
	}
}

func TestSiteProxyRoutesThroughHelper(t *testing.T) {
	previous := PrivilegedCall
	t.Cleanup(func() { PrivilegedCall = previous })
	var got helper.Request
	PrivilegedCall = func(_ context.Context, req helper.Request) (string, error) {
		if req.Action != "proxy-apply" {
			return "", errors.New("unexpected action " + req.Action)
		}
		got = req
		return "", nil
	}
	m := NewCertManager(&SiteManager{
		Run:        func(context.Context, string, ...string) (string, error) { return "", nil },
		RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) { return "", nil },
	}, nil, certs.NewStore(t.TempDir()))
	m.loadSite = func(string) (*managedSite, error) {
		return &managedSite{
			ID: "/etc/nginx/sites-enabled/app.conf", Engine: "nginx", Kind: "proxy",
			Domain: "app.example.com", Port: 80, SiteName: "app", ProxyPass: "http://127.0.0.1:3000",
		}, nil
	}
	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/sites/proxy", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		m.SiteProxy(w, r)
		return w
	}
	nodes := `[{"target":"http://10.0.0.1:8080","weight":3},{"target":"http://10.0.0.2:8080","backup":true}]`
	if w := post(url.Values{"id": {"/etc/nginx/sites-enabled/app.conf"}, "nodes": {nodes}, "websocket": {"true"}}); w.Code != 200 {
		t.Fatalf("proxy apply failed: %d %s", w.Code, w.Body.String())
	}
	if got.Site != "app" || got.Kind != "proxy" || !got.WebSocket ||
		len(got.ProxyNodes) != 2 || got.ProxyNodes[0].Weight != 3 || !got.ProxyNodes[1].Backup {
		t.Fatalf("wrong helper request: %+v", got)
	}
	// invalid node payloads are refused before any privileged call
	PrivilegedCall = func(_ context.Context, _ helper.Request) (string, error) {
		t.Error("helper must not be called for invalid input")
		return "", nil
	}
	for _, bad := range []string{`[]`, `["not-an-object"]`, `[{"target":"ftp://x"}]`} {
		if w := post(url.Values{"id": {"/etc/nginx/sites-enabled/app.conf"}, "nodes": {bad}}); w.Code != 400 {
			t.Errorf("invalid nodes %q accepted: %d", bad, w.Code)
		}
	}
	// static sites are refused
	m.loadSite = func(string) (*managedSite, error) {
		return &managedSite{ID: "/etc/nginx/sites-enabled/blog.conf", Engine: "nginx", Kind: "static", SiteName: "blog"}, nil
	}
	if w := post(url.Values{"id": {"/etc/nginx/sites-enabled/blog.conf"}, "nodes": {`[{"target":"http://a:1"}]`}}); w.Code != 400 {
		t.Fatalf("static site must be refused: %d", w.Code)
	}
}

func TestParseNginxAdvancedSettings(t *testing.T) {
	dir := t.TempDir()
	m := &SiteManager{Run: func(context.Context, string, ...string) (string, error) { return "", nil }}
	write := func(name string, spec helper.SiteSpec) {
		t.Helper()
		conf, err := helper.RenderSite(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("multi.conf", helper.SiteSpec{Name: "multi", Engine: "nginx", Kind: "static",
		Domains: []string{"a.example.com", "b.example.com", "*.c.example.com"},
		Port:    80, Root: "/var/www/multi", Index: []string{"home.html", "index.htm"}})
	write("spa.conf", helper.SiteSpec{Name: "spa", Engine: "nginx", Kind: "static",
		Domains: []string{"spa.example.com"}, Port: 80, Root: "/var/www/spa", Rewrite: "spa"})
	write("redir.conf", helper.SiteSpec{Name: "redir", Engine: "nginx", Kind: "static",
		Domains: []string{"old.example.com"}, Port: 80, Root: "/var/www/old",
		Redirect: &helper.Redirect{Target: "https://new.example.com", Code: 301, KeepPath: true}})
	write("custom.conf", helper.SiteSpec{Name: "custom", Engine: "nginx", Kind: "static",
		Domains: []string{"custom.example.com"}, Port: 80, Root: "/var/www/custom",
		Rewrite: "custom", RewriteBody: "deny 192.0.2.0/24;\ntry_files $uri $uri/ /index.html;"})
	sites := m.confDirSites(context.Background(), []string{dir}, "nginx", true)
	byName := map[string]Site{}
	for _, s := range sites {
		byName[s.Detail] = s
	}
	if len(byName) != 4 {
		t.Fatalf("want 4 sites, got %d: %v", len(byName), byName)
	}
	multi := byName["multi.conf"]
	if !reflect.DeepEqual(multi.ServerNames, []string{"a.example.com", "b.example.com", "*.c.example.com"}) ||
		!reflect.DeepEqual(multi.Index, []string{"home.html", "index.htm"}) {
		t.Fatalf("multi-domain site parsed wrong: %+v", multi)
	}
	if spa := byName["spa.conf"]; spa.Rewrite != "spa" || spa.RewriteBody != "" {
		t.Fatalf("spa preset parsed wrong: %+v", spa)
	}
	redir := byName["redir.conf"]
	if redir.RedirectTarget != "https://new.example.com" || redir.RedirectCode != 301 || !redir.RedirectKeepPath {
		t.Fatalf("redirect parsed wrong: %+v", redir)
	}
	custom := byName["custom.conf"]
	if custom.Rewrite != "custom" || !strings.Contains(custom.RewriteBody, "deny 192.0.2.0/24;") {
		t.Fatalf("custom rewrite parsed wrong: %+v", custom)
	}
}

func TestParseApacheAdvancedSettings(t *testing.T) {
	dir := t.TempDir()
	m := &SiteManager{Run: func(context.Context, string, ...string) (string, error) { return "", nil }}
	write := func(name string, spec helper.SiteSpec) {
		t.Helper()
		conf, err := helper.RenderSite(spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("blog.conf", helper.SiteSpec{Name: "blog", Engine: "apache", Kind: "static",
		Domains: []string{"a.example.com", "b.example.com"}, Port: 80, Root: "/var/www/blog",
		Index:    []string{"home.html"},
		Redirect: &helper.Redirect{Target: "https://new.example.com", Code: 302}})
	// 强制 HTTPS 的 RedirectMatch 跳到自己域名，不能被误读为站点重定向
	write("force.conf", helper.SiteSpec{Name: "force", Engine: "apache", Kind: "static",
		Domains: []string{"force.example.com"}, Port: 80, Root: "/var/www/force",
		SSL: helper.SSLConf{Enabled: true, ForceHTTPS: true,
			CertFile: "/var/lib/lightpanel/acme/certs/force.example.com/cert.pem",
			KeyFile:  "/var/lib/lightpanel/acme/certs/force.example.com/privkey.pem"}})
	sites := m.confDirSites(context.Background(), []string{dir}, "apache", true)
	byName := map[string]Site{}
	for _, s := range sites {
		byName[s.Detail] = s
	}
	blog := byName["blog.conf"]
	if !reflect.DeepEqual(blog.ServerNames, []string{"a.example.com", "b.example.com"}) ||
		!reflect.DeepEqual(blog.Index, []string{"home.html"}) ||
		blog.RedirectTarget != "https://new.example.com" || blog.RedirectCode != 302 || blog.RedirectKeepPath {
		t.Fatalf("apache settings parsed wrong: %+v", blog)
	}
	force := byName["force.conf"]
	if force.RedirectTarget != "" {
		t.Fatalf("force-HTTPS rule must not parse as site redirect: %+v", force)
	}
	if !force.SSL {
		t.Fatalf("SSL flag lost: %+v", force)
	}
}

func TestDomainConflicts(t *testing.T) {
	if !domainOverlap("a.example.com", "A.Example.COM") || domainOverlap("a.example.com", "b.example.com") {
		t.Fatal("domainOverlap exact match broken")
	}
	if !domainOverlap("app.example.com", "*.example.com") || !domainOverlap("*.example.com", "app.example.com") {
		t.Fatal("domainOverlap wildcard broken")
	}
	if domainOverlap("app.example.com", "*.other.com") {
		t.Fatal("unrelated hosts must not overlap")
	}

	previous := siteScanDirs
	t.Cleanup(func() { siteScanDirs = previous })
	nginxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(nginxDir, "other.conf"), []byte(
		helper.ManagedMarker+"\nserver {\n    listen 80;\n    server_name taken.example.com *.taken-wild.com;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	siteScanDirs = map[string][]string{"nginx": {nginxDir}, "apache": {t.TempDir()}}
	m := &CertManager{sites: &SiteManager{Run: func(context.Context, string, ...string) (string, error) { return "", nil }}}
	conflicts := m.domainConflicts(context.Background(), "/etc/nginx/sites-enabled/mine.conf",
		[]string{"free.example.com", "taken.example.com", "sub.taken-wild.com"})
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %v", conflicts)
	}
	if free := m.domainConflicts(context.Background(), "/etc/nginx/sites-enabled/mine.conf", []string{"free.example.com"}); len(free) != 0 {
		t.Fatalf("free domains must not conflict: %v", free)
	}
}

func TestSiteConfRoutesThroughHelper(t *testing.T) {
	previous := PrivilegedCall
	previousDirs := siteScanDirs
	t.Cleanup(func() { PrivilegedCall = previous; siteScanDirs = previousDirs })
	siteScanDirs = map[string][]string{"nginx": {t.TempDir()}, "apache": {t.TempDir()}}
	var got helper.Request
	PrivilegedCall = func(_ context.Context, req helper.Request) (string, error) {
		if req.Action != "conf-apply" {
			return "", errors.New("unexpected action " + req.Action)
		}
		got = req
		return "", nil
	}
	m := NewCertManager(&SiteManager{
		Run:        func(context.Context, string, ...string) (string, error) { return "", nil },
		RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) { return "", nil },
	}, nil, certs.NewStore(t.TempDir()))
	m.loadSite = func(string) (*managedSite, error) {
		return &managedSite{
			ID: "/etc/nginx/sites-enabled/blog.conf", Engine: "nginx", Kind: "static",
			Domain: "blog.example.com", Port: 80, Root: "/var/www/blog", SiteName: "blog",
			ServerName: "blog.example.com",
		}, nil
	}
	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/sites/conf", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		m.SiteConf(w, r)
		return w
	}
	if w := post(url.Values{
		"id":                 {"/etc/nginx/sites-enabled/blog.conf"},
		"domains":            {`["blog.example.com","www.blog.example.com"]`},
		"index":              {"home.html\nindex.htm"},
		"redirect_to":        {"https://parked.example.com"},
		"redirect_code":      {"302"},
		"redirect_keep_path": {"true"},
		"rewrite":            {"spa"},
	}); w.Code != 200 {
		t.Fatalf("conf apply failed: %d %s", w.Code, w.Body.String())
	}
	if len(got.Domains) != 2 || !reflect.DeepEqual(got.Index, []string{"home.html", "index.htm"}) ||
		got.RedirectTarget != "https://parked.example.com" || got.RedirectCode != 302 || !got.RedirectKeepPath ||
		got.Rewrite != "spa" {
		t.Fatalf("wrong helper request: %+v", got)
	}
	// 域名被其他站点绑定时拒绝（且不打 helper）
	scanDir := siteScanDirs["nginx"][0]
	if err := os.WriteFile(filepath.Join(scanDir, "other.conf"), []byte(
		helper.ManagedMarker+"\nserver {\n    listen 80;\n    server_name taken.example.com;\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	PrivilegedCall = func(_ context.Context, _ helper.Request) (string, error) {
		t.Error("helper must not be called when domains conflict")
		return "", nil
	}
	if w := post(url.Values{
		"id":      {"/etc/nginx/sites-enabled/blog.conf"},
		"domains": {`["taken.example.com"]`},
	}); w.Code != 400 {
		t.Fatalf("conflicting domain must be refused: %d %s", w.Code, w.Body.String())
	}
	// 非法域名拒绝
	if w := post(url.Values{"id": {"/etc/nginx/sites-enabled/blog.conf"}, "domains": {`["bad domain"]`}}); w.Code != 400 {
		t.Fatalf("invalid domain must be refused: %d", w.Code)
	}
}

func TestSiteActionDeleteNativeRoutesThroughHelper(t *testing.T) {
	previous := PrivilegedCall
	t.Cleanup(func() { PrivilegedCall = previous })
	var got helper.Request
	PrivilegedCall = func(_ context.Context, req helper.Request) (string, error) {
		if req.Action != "delete" && req.Action != "reload" {
			return "", errors.New("unexpected action " + req.Action)
		}
		if req.Action == "delete" {
			got = req
		}
		return "", nil
	}
	m := &SiteManager{Run: func(_ context.Context, _ string, args ...string) (string, error) {
		// reloadEngine resolves the unit after deletion
		if args[0] == "list-unit-files" {
			return "nginx.service enabled\n", nil
		}
		return "", nil
	}, RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) { return "", nil }}
	r := httptest.NewRequest("POST", "/api/sites/action", strings.NewReader(url.Values{"id": {"/etc/nginx/conf.d/blog.conf"}, "op": {"delete"}, "engine": {"nginx"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	m.SiteAction(w, r)
	if w.Code != 200 {
		t.Fatalf("delete failed: %d %s", w.Code, w.Body.String())
	}
	if got.Op != helper.OpSite || got.Path != "/etc/nginx/conf.d/blog.conf" {
		t.Fatalf("wrong helper delete request: %+v", got)
	}
}

// Let's Encrypt issuance moved to pkg/sysinfo/certs.go with the built-in
// ACME client; its tests live in certs_test.go.
