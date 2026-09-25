package helper

import (
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
