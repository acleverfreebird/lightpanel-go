package helper

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSiteNameAndImageValidation(t *testing.T) {
	for _, name := range []string{"blog", "a1", "my-site-2"} {
		if !ValidSiteName(name) {
			t.Errorf("ValidSiteName(%q) rejected", name)
		}
	}
	for _, name := range []string{"", "-a", "UPPER", "a b", "site.name", strings.Repeat("a", 40)} {
		if ValidSiteName(name) {
			t.Errorf("ValidSiteName(%q) accepted", name)
		}
	}
	if !ValidImageRef("nginx") || !ValidImageRef("ghcr.io/owner/app:v1.0") {
		t.Error("valid image refs rejected")
	}
	for _, img := range []string{"", "nginx; rm -rf /", "a..b", "nginx:bad tag"} {
		if ValidImageRef(img) {
			t.Errorf("ValidImageRef(%q) accepted", img)
		}
	}
}

func TestValidServerName(t *testing.T) {
	if !ValidServerName("_") || !ValidServerName("*.example.com") || !ValidServerName("a-b.example.co.uk") || !ValidServerName("127.0.0.1") {
		t.Error("valid names rejected")
	}
	for _, d := range []string{"", "-a.b", "a-.b", "bad..name", "x y.z", "*.com-"} {
		if ValidServerName(d) {
			t.Errorf("ValidServerName(%q) accepted", d)
		}
	}
}

func TestValidProxyTarget(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1:3000", "https://upstream.internal", "http://app:8080/api/", "https://a-b.example.com"} {
		if !ValidProxyTarget(target) {
			t.Errorf("ValidProxyTarget(%q) rejected", target)
		}
	}
	for _, target := range []string{
		"", "ftp://x", "http://", "http://x:99999", "http://user:pass@host",
		"http://host/path?q=1", "javascript:alert(1)", "http://host/#frag", "http://x..y",
	} {
		if ValidProxyTarget(target) {
			t.Errorf("ValidProxyTarget(%q) accepted", target)
		}
	}
}

func TestValidEmailAndDocumentRoot(t *testing.T) {
	if !ValidEmail("admin@example.com") || !ValidEmail("a.b+c@sub.example.co") {
		t.Error("valid emails rejected")
	}
	for _, e := range []string{"", "not-an-email", "a@b", "a b@c.d", "@x.y", "a@" + strings.Repeat("x", 250) + ".com"} {
		if ValidEmail(e) {
			t.Errorf("ValidEmail(%q) accepted", e)
		}
	}
	if !ValidDocumentRoot("/var/www/blog") || !ValidDocumentRoot("/srv/app.dist") {
		t.Error("valid roots rejected")
	}
	for _, root := range []string{"", "/", "var/www", "/var/../etc", "/var//www", "/var/www/", "C:\\www"} {
		if ValidDocumentRoot(root) {
			t.Errorf("ValidDocumentRoot(%q) accepted", root)
		}
	}
}

func TestSiteConfErrors(t *testing.T) {
	cases := []struct {
		name, engine, kind, domain, root, proxy string
		port                                    int
	}{
		{"bad engine", "caddy", "static", "a.com", "/var/www/a", "", 80},
		{"bad kind", "nginx", "redirect", "a.com", "/var/www/a", "", 80},
		{"bad port", "nginx", "static", "a.com", "/var/www/a", "", 0},
		{"bad domain", "nginx", "static", "a b", "/var/www/a", "", 80},
		{"proxy needs target", "nginx", "proxy", "a.com", "", "", 80},
		{"static needs root", "nginx", "static", "a.com", "", "", 80},
		{"bad proxy target", "nginx", "proxy", "a.com", "", "http://x:0", 80},
	}
	for _, c := range cases {
		if _, err := SiteConf(c.engine, c.kind, "site", c.domain, c.port, c.root, c.proxy); err == nil {
			t.Errorf("%s: SiteConf accepted invalid input", c.name)
		}
	}
}

func TestIsManagedConfPath(t *testing.T) {
	for _, p := range []string{"/etc/nginx/sites-enabled/blog.conf", "/etc/nginx/conf.d/x.conf", "/etc/httpd/conf.d/x.conf", "/etc/apache2/sites-available/x.conf"} {
		if !IsManagedConfPath(p) {
			t.Errorf("IsManagedConfPath(%q) rejected", p)
		}
	}
	for _, p := range []string{"/etc/nginx/nginx.conf", "/var/www/x.conf", "relative.conf", "/etc/nginx/conf.d/../evil.conf", "/etc/other/x.conf"} {
		if IsManagedConfPath(p) {
			t.Errorf("IsManagedConfPath(%q) accepted", p)
		}
	}
}

const (
	stockNginxDefault = `server {
	listen 80 default_server;
	listen [::]:80 default_server;
	root /var/www/html;
	index index.html index.htm index.nginx-debian.html;
	server_name _;
}
`
	stockApacheDefault = `<VirtualHost *:80>
	ServerAdmin webmaster@localhost
	DocumentRoot /var/www/html
</VirtualHost>
`
)

// takeOverLayout redirects every distro-default path into a temporary
// directory and restores the package vars afterwards.
func takeOverLayout(t *testing.T, seed func(enabled, available string)) map[string]distroDefaultSite {
	t.Helper()
	root := t.TempDir()
	enabled := filepath.Join(root, "sites-enabled")
	available := filepath.Join(root, "sites-available")
	for _, dir := range []string{enabled, available} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	previousSites, previousDir, previousMain := distroDefaultSites, nginxSitesEnabledDir, nginxMainConf
	t.Cleanup(func() { distroDefaultSites, nginxSitesEnabledDir, nginxMainConf = previousSites, previousDir, previousMain })
	distroDefaultSites = map[string]distroDefaultSite{
		"nginx":  {Enabled: filepath.Join(enabled, "default"), Available: filepath.Join(available, "default")},
		"apache": {Enabled: filepath.Join(enabled, "000-default.conf"), Available: filepath.Join(available, "000-default.conf")},
	}
	nginxSitesEnabledDir = enabled
	nginxMainConf = filepath.Join(root, "nginx.conf")
	if seed != nil {
		seed(enabled, available)
	}
	return distroDefaultSites
}

func TestProxyConfRendering(t *testing.T) {
	nodes := []ProxyNode{
		{Target: "http://10.0.0.1:8080", Weight: 3},
		{Target: "http://10.0.0.2:8080", Backup: true},
	}
	conf, err := SiteConfEx("nginx", "proxy", "app", "app.example.com", 80, "",
		ProxyConf{Nodes: nodes, WebSocket: true}, SSLConf{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"upstream lightpanel-app {",
		"server 10.0.0.1:8080 weight=3;",
		"server 10.0.0.2:8080 backup;",
		"proxy_pass http://lightpanel-app;",
		"map $http_upgrade lightpanel_app_ws {",
		"proxy_http_version 1.1;",
		"proxy_set_header Upgrade $http_upgrade;",
		"proxy_set_header Connection $lightpanel_app_ws;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("multi-node conf missing %q:\n%s", want, conf)
		}
	}
	// single node keeps the classic template: no upstream group, no map
	single, err := SiteConfEx("nginx", "proxy", "app", "app.example.com", 80, "",
		ProxyConf{Nodes: []ProxyNode{{Target: "http://127.0.0.1:3000"}}}, SSLConf{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(single, "upstream ") || strings.Contains(single, "map ") || strings.Contains(single, "Upgrade") {
		t.Errorf("single-node conf must stay minimal:\n%s", single)
	}
	if !strings.Contains(single, "proxy_pass http://127.0.0.1:3000;") {
		t.Errorf("single-node conf wrong:\n%s", single)
	}
	apache, err := SiteConfEx("apache", "proxy", "app", "app.example.com", 80, "",
		ProxyConf{Nodes: nodes}, SSLConf{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<Proxy balancer://lightpanel-app>",
		"BalancerMember http://10.0.0.1:8080 loadfactor=3",
		"BalancerMember http://10.0.0.2:8080 status=+H",
		"ProxyPass / balancer://lightpanel-app/",
	} {
		if !strings.Contains(apache, want) {
			t.Errorf("apache conf missing %q:\n%s", want, apache)
		}
	}
	// the balancer group lives at server level and must be defined exactly
	// once even when the site has both the 80 and the 443 virtual host
	withSSL, err := SiteConfEx("apache", "proxy", "app", "app.example.com", 80, "",
		ProxyConf{Nodes: nodes}, SSLConf{
			Enabled:  true,
			CertFile: "/var/lib/lightpanel/acme/certs/app.example.com/cert.pem",
			KeyFile:  "/var/lib/lightpanel/acme/certs/app.example.com/privkey.pem",
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(withSSL, "<Proxy "); got != 1 {
		t.Errorf("balancer group must be defined once, got %d:\n%s", got, withSSL)
	}
}

func TestProxyConfValidation(t *testing.T) {
	if err := ValidProxyConf("nginx", ProxyConf{Nodes: []ProxyNode{{Target: "http://a:1"}}}); err != nil {
		t.Errorf("valid conf rejected: %v", err)
	}
	many := make([]ProxyNode, MaxProxyNodes+1)
	for i := range many {
		many[i] = ProxyNode{Target: "http://10.0.0." + strconv.Itoa(i) + ":8080"}
	}
	for _, c := range []struct {
		name   string
		engine string
		proxy  ProxyConf
	}{
		{"no nodes", "nginx", ProxyConf{}},
		{"too many nodes", "nginx", ProxyConf{Nodes: many}},
		{"bad target", "nginx", ProxyConf{Nodes: []ProxyNode{{Target: "ftp://x"}}}},
		{"weight over 100", "nginx", ProxyConf{Nodes: []ProxyNode{{Target: "http://a:1", Weight: 101}}}},
		{"duplicate target", "nginx", ProxyConf{Nodes: []ProxyNode{{Target: "http://a:1"}, {Target: "http://a:1"}}}},
		{"mixed schemes", "nginx", ProxyConf{Nodes: []ProxyNode{{Target: "http://a:1"}, {Target: "https://b:1"}}}},
		{"path on any node", "nginx", ProxyConf{Nodes: []ProxyNode{{Target: "http://a:1"}, {Target: "http://b:1/api"}}}},
		{"websocket on apache", "apache", ProxyConf{Nodes: []ProxyNode{{Target: "http://a:1"}}, WebSocket: true}},
	} {
		if err := ValidProxyConf(c.engine, c.proxy); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

func TestRenderSiteAdvanced(t *testing.T) {
	// 多域名：nginx 合并 server_name；Apache ServerName + ServerAlias
	nginx, err := RenderSite(SiteSpec{Name: "multi", Engine: "nginx", Kind: "static",
		Domains: []string{"a.example.com", "b.example.com", "*.c.example.com"},
		Port: 80, Root: "/var/www/multi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(nginx, "server_name a.example.com b.example.com *.c.example.com;") {
		t.Errorf("multi-domain server_name wrong:\n%s", nginx)
	}
	apache, err := RenderSite(SiteSpec{Name: "multi", Engine: "apache", Kind: "static",
		Domains: []string{"a.example.com", "b.example.com"},
		Port: 80, Root: "/var/www/multi"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(apache, "ServerName a.example.com\n") || !strings.Contains(apache, "ServerAlias b.example.com\n") {
		t.Errorf("apache domain binding wrong:\n%s", apache)
	}

	// 默认文档：空列表保持经典模板；自定义顺序生效（nginx + Apache）
	def, _ := RenderSite(SiteSpec{Name: "d", Engine: "nginx", Kind: "static", Port: 80, Root: "/var/www/d"})
	if !strings.Contains(def, "index index.html index.htm;") {
		t.Errorf("default index lost:\n%s", def)
	}
	idx, _ := RenderSite(SiteSpec{Name: "d", Engine: "nginx", Kind: "static", Port: 80, Root: "/var/www/d", Index: []string{"home.html", "index.htm"}})
	if !strings.Contains(idx, "index home.html index.htm;") {
		t.Errorf("custom index lost:\n%s", idx)
	}
	aidx, _ := RenderSite(SiteSpec{Name: "d", Engine: "apache", Kind: "static", Port: 80, Root: "/var/www/d", Index: []string{"home.html"}})
	if !strings.Contains(aidx, "DirectoryIndex home.html") {
		t.Errorf("apache DirectoryIndex lost:\n%s", aidx)
	}

	// 整站重定向：nginx keep/drop 路径；80 与 443 块都跳转
	keep, err := RenderSite(SiteSpec{Name: "r", Engine: "nginx", Kind: "static",
		Port: 80, Root: "/var/www/r", Redirect: &Redirect{Target: "https://new.example.com", Code: 301, KeepPath: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(keep, "return 301 https://new.example.com$request_uri;") {
		t.Errorf("keep-path redirect wrong:\n%s", keep)
	}
	keepSSL, err := RenderSite(SiteSpec{Name: "r", Engine: "nginx", Kind: "static",
		Port: 80, Root: "/var/www/r", Redirect: &Redirect{Target: "https://new.example.com", Code: 302},
		SSL: SSLConf{Enabled: true, ForceHTTPS: true,
			CertFile: "/var/lib/lightpanel/acme/certs/r.example.com/cert.pem",
			KeyFile:  "/var/lib/lightpanel/acme/certs/r.example.com/privkey.pem"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(keepSSL, "return 302 https://new.example.com;"); got != 2 {
		t.Errorf("redirect must appear in both server blocks, got %d:\n%s", got, keepSSL)
	}
	if strings.Contains(keepSSL, "https://$host") {
		t.Errorf("explicit redirect must win over force-HTTPS:\n%s", keepSSL)
	}
	akeep, err := RenderSite(SiteSpec{Name: "r", Engine: "apache", Kind: "static",
		Port: 80, Root: "/var/www/r", Redirect: &Redirect{Target: "https://new.example.com", Code: 301, KeepPath: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(akeep, "RedirectMatch 301 ^/(?!\\.well-known/acme-challenge/)(.*)$ https://new.example.com/$1") {
		t.Errorf("apache keep-path redirect wrong:\n%s", akeep)
	}

	// 伪静态：SPA 与自定义 location 内容
	spa, _ := RenderSite(SiteSpec{Name: "s", Engine: "nginx", Kind: "static", Port: 80, Root: "/var/www/s", Rewrite: "spa"})
	if !strings.Contains(spa, "try_files $uri $uri/ /index.html;") {
		t.Errorf("spa rewrite lost:\n%s", spa)
	}
	custom, err := RenderSite(SiteSpec{Name: "c", Engine: "nginx", Kind: "static", Port: 80, Root: "/var/www/c",
		Rewrite: "custom", RewriteBody: "deny 192.0.2.0/24;\ntry_files $uri $uri/ /index.html;"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(custom, "        deny 192.0.2.0/24;\n        try_files $uri $uri/ /index.html;") {
		t.Errorf("custom rewrite body not embedded:\n%s", custom)
	}
	// 空域名列表 = 通配站点 "_"
	wild, _ := RenderSite(SiteSpec{Name: "w", Engine: "nginx", Kind: "static", Port: 80, Root: "/var/www/w"})
	if !strings.Contains(wild, "server_name _;") {
		t.Errorf("wildcard binding lost:\n%s", wild)
	}
}

func TestSiteSpecValidation(t *testing.T) {
	base := SiteSpec{Name: "ok", Engine: "nginx", Kind: "static", Port: 80, Root: "/var/www/ok"}
	if err := base.validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*SiteSpec)
	}{
		{"duplicate domain", func(s *SiteSpec) { s.Domains = []string{"a.com", "a.com"} }},
		{"too many domains", func(s *SiteSpec) {
			for i := 0; i <= MaxSiteDomains; i++ {
				s.Domains = append(s.Domains, fmt.Sprintf("d%d.example.com", i))
			}
		}},
		{"invalid domain", func(s *SiteSpec) { s.Domains = []string{"a b.com"} }},
		{"bad index entry", func(s *SiteSpec) { s.Index = []string{"../etc/passwd"} }},
		{"too many index entries", func(s *SiteSpec) {
			for i := 0; i < 9; i++ {
				s.Index = append(s.Index, fmt.Sprintf("f%d.html", i))
			}
		}},
		{"bad redirect code", func(s *SiteSpec) { s.Redirect = &Redirect{Target: "https://t.com", Code: 300} }},
		{"bad redirect target", func(s *SiteSpec) { s.Redirect = &Redirect{Target: "ftp://t.com", Code: 301} }},
		{"unknown rewrite", func(s *SiteSpec) { s.Rewrite = "wordpress" }},
		{"rewrite on proxy", func(s *SiteSpec) { s.Kind = "proxy"; s.Proxy = ProxyConf{Nodes: []ProxyNode{{Target: "http://a:1"}}}; s.Rewrite = "spa" }},
		{"custom rewrite on apache", func(s *SiteSpec) { s.Engine = "apache"; s.Rewrite = "custom"; s.RewriteBody = "x" }},
		{"oversized rewrite body", func(s *SiteSpec) { s.Rewrite = "custom"; s.RewriteBody = strings.Repeat("x", maxRewriteBytes+1) }},
	}
	for _, c := range cases {
		spec := base
		c.mut(&spec)
		if err := spec.validate(); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

func TestTakeOverDefaultSite(t *testing.T) {
	t.Run("removes the stock nginx default site", func(t *testing.T) {
		sites := takeOverLayout(t, func(enabled, _ string) {
			if err := os.WriteFile(filepath.Join(enabled, "default"), []byte(stockNginxDefault), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		note := TakeOverDefaultSite("nginx", 80)
		if !strings.Contains(note, "已停用") {
			t.Fatalf("expected removal note, got %q", note)
		}
		if _, err := os.Lstat(sites["nginx"].Enabled); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("default site still enabled: %v", err)
		}
	})

	t.Run("follows the distribution symlink and keeps the source", func(t *testing.T) {
		sites := takeOverLayout(t, func(_, available string) {
			if err := os.WriteFile(filepath.Join(available, "default"), []byte(stockNginxDefault), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		d := sites["nginx"]
		if err := os.Symlink("../sites-available/default", d.Enabled); err != nil {
			t.Skip("symlinks unavailable:", err)
		}
		if note := TakeOverDefaultSite("nginx", 80); !strings.Contains(note, "已停用") {
			t.Fatalf("expected removal note, got %q", note)
		}
		if _, err := os.Lstat(d.Enabled); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("symlink still present: %v", err)
		}
		if _, err := os.Stat(d.Available); err != nil {
			t.Fatalf("sites-available source must survive: %v", err)
		}
	})

	t.Run("never touches a custom site sharing the name", func(t *testing.T) {
		sites := takeOverLayout(t, func(enabled, _ string) {
			custom := "server {\n\tlisten 80 default_server;\n\troot /srv/custom;\n}\n"
			if err := os.WriteFile(filepath.Join(enabled, "default"), []byte(custom), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		note := TakeOverDefaultSite("nginx", 80)
		if note == "" || !strings.Contains(note, "未改动") {
			t.Fatalf("expected leave-alone note, got %q", note)
		}
		if _, err := os.Stat(sites["nginx"].Enabled); err != nil {
			t.Fatalf("custom configuration was removed: %v", err)
		}
	})

	t.Run("ignores ports other than 80 and absent sites", func(t *testing.T) {
		sites := takeOverLayout(t, func(enabled, _ string) {
			if err := os.WriteFile(filepath.Join(enabled, "default"), []byte(stockNginxDefault), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		if note := TakeOverDefaultSite("nginx", 8080); note != "" {
			t.Fatalf("port 8080 must not trigger takeover: %q", note)
		}
		if _, err := os.Stat(sites["nginx"].Enabled); err != nil {
			t.Fatalf("default site was removed on an unrelated port: %v", err)
		}
		if note := TakeOverDefaultSite("caddy", 80); note != "" {
			t.Fatalf("unknown engine must be a no-op: %q", note)
		}
	})

	t.Run("no default site installed", func(t *testing.T) {
		takeOverLayout(t, nil)
		if note := TakeOverDefaultSite("nginx", 80); note != "" {
			t.Fatalf("absent default site must be a no-op: %q", note)
		}
	})

	t.Run("removes the stock apache default site", func(t *testing.T) {
		sites := takeOverLayout(t, func(enabled, _ string) {
			if err := os.WriteFile(filepath.Join(enabled, "000-default.conf"), []byte(stockApacheDefault), 0o644); err != nil {
				t.Fatal(err)
			}
		})
		if note := TakeOverDefaultSite("apache", 80); !strings.Contains(note, "已停用") {
			t.Fatalf("expected removal note, got %q", note)
		}
		if _, err := os.Lstat(sites["apache"].Enabled); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("default site still enabled: %v", err)
		}
	})

	t.Run("warns on the RHEL layout instead of touching nginx.conf", func(t *testing.T) {
		sites := takeOverLayout(t, nil)
		if err := os.Remove(filepath.Dir(sites["nginx"].Enabled)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(nginxMainConf, []byte("server {\n\tlisten 80 default_server;\n\troot /usr/share/nginx/html;\n}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		note := TakeOverDefaultSite("nginx", 80)
		if !strings.Contains(note, "nginx.conf") || !strings.Contains(note, "default_server") {
			t.Fatalf("expected a main-config warning, got %q", note)
		}
	})
}
