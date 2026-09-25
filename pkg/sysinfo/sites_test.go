package sysinfo

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

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
	if err := m.createDockerSite(context.Background(), "blog", "nginx:1.27", 8080, 80); err != nil {
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
	if err := m.createDockerSite(context.Background(), "blog", "nginx", 80, 80); !errors.Is(err, errConflict) {
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
	if err := m.createNativeSite(context.Background(), "nginx", "proxy", "app", "app.example.com", 8080, "", "http://127.0.0.1:3000"); err != nil {
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
	if err := m.createNativeSite(context.Background(), "nginx", "static", "blog", "", 80, "", ""); !errors.Is(err, errConflict) {
		t.Fatalf("conflict not mapped: %v", err)
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

func TestIssueCert(t *testing.T) {
	previous := PrivilegedCall
	t.Cleanup(func() { PrivilegedCall = previous })
	var got helper.Request
	PrivilegedCall = func(_ context.Context, req helper.Request) (string, error) {
		got = req
		return "Successfully received certificate.\n", nil
	}
	m := &SiteManager{Run: func(context.Context, string, ...string) (string, error) { return "", nil },
		RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) { return "", nil }}
	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/sites/cert", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		m.IssueCert(w, r)
		return w
	}
	if w := post(url.Values{"domain": {"*.example.com"}, "email": {"a@b.co"}, "engine": {"nginx"}}); w.Code != 400 {
		t.Fatalf("wildcard accepted: %d", w.Code)
	}
	if w := post(url.Values{"domain": {"blog.example.com"}, "email": {"not-an-email"}, "engine": {"nginx"}}); w.Code != 400 {
		t.Fatalf("bad email accepted: %d", w.Code)
	}
	if w := post(url.Values{"domain": {"blog.example.com"}, "email": {"a@b.co"}, "engine": {"docker"}}); w.Code != 400 {
		t.Fatalf("docker engine accepted: %d", w.Code)
	}
	if w := post(url.Values{"domain": {"blog.example.com"}, "email": {"a@b.co"}, "engine": {"nginx"}}); w.Code != 200 {
		t.Fatalf("valid issuance failed: %d %s", w.Code, w.Body.String())
	}
	if got.Op != helper.OpSite || got.Action != "issue-cert" || got.Domain != "blog.example.com" || got.Email != "a@b.co" || got.Engine != "nginx" {
		t.Fatalf("wrong helper cert request: %+v", got)
	}
}

func TestCertificatesEndpoint(t *testing.T) {
	previous := PrivilegedCall
	t.Cleanup(func() { PrivilegedCall = previous })
	PrivilegedCall = nil
	m := &SiteManager{Run: func(_ context.Context, command string, args ...string) (string, error) {
		if command == "certbot" {
			return "Saving debug log to /var/log/letsencrypt\nCertificate Name: blog\n", nil
		}
		return "", nil
	}, RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) { return "", nil }}
	w := httptest.NewRecorder()
	m.Certificates(w, httptest.NewRequest("GET", "/api/sites/certs", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Certificate Name: blog") {
		t.Fatalf("cert listing failed: %d %s", w.Code, w.Body.String())
	}
	// certbot missing entirely → 501 with installed=false
	m.Run = func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", ErrUnavailable
	}
	w = httptest.NewRecorder()
	m.Certificates(w, httptest.NewRequest("GET", "/api/sites/certs", nil))
	if w.Code != 501 || !strings.Contains(w.Body.String(), "\"installed\":false") {
		t.Fatalf("missing certbot must be 501: %d %s", w.Code, w.Body.String())
	}
}
