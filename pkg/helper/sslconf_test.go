package helper

import (
	"strings"
	"testing"
)

func TestSiteConfExNginxSSL(t *testing.T) {
	conf, err := SiteConfEx("nginx", "static", "blog", "blog.example.com", 80, "/var/www/blog", ProxyConf{}, SSLConf{
		Enabled: true, ForceHTTPS: true, Challenge: true,
		CertFile: "/var/lib/lightpanel/acme/certs/blog.example.com/fullchain.pem",
		KeyFile:  "/var/lib/lightpanel/acme/certs/blog.example.com/privkey.pem",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.HasPrefix(conf, ManagedMarker) {
		t.Error("rendered conf must carry the managed marker")
	}
	for _, want := range []string{
		"listen 80;",
		"listen 443 ssl;",
		"ssl_certificate /var/lib/lightpanel/acme/certs/blog.example.com/fullchain.pem;",
		"ssl_certificate_key /var/lib/lightpanel/acme/certs/blog.example.com/privkey.pem;",
		"location ^~ /.well-known/acme-challenge/",
		"return 301 https://$host$request_uri;",
		"root /var/www/blog;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf missing %q\n%s", want, conf)
		}
	}
	// The challenge location must come before the redirect location so the
	// ACME path stays reachable while force-HTTPS is on.
	if strings.Index(conf, "acme-challenge") > strings.Index(conf, "return 301") {
		t.Errorf("challenge location must precede the redirect\n%s", conf)
	}
}

func TestSiteConfExNginxSSLNoForce(t *testing.T) {
	conf, err := SiteConfEx("nginx", "proxy", "app", "app.example.com", 80, "",
		ProxyConf{Nodes: []ProxyNode{{Target: "http://127.0.0.1:3000"}}}, SSLConf{
		Enabled:   true,
		Challenge: true,
		CertFile:  "/var/lib/lightpanel/acme/certs/app.example.com/fullchain.pem",
		KeyFile:   "/var/lib/lightpanel/acme/certs/app.example.com/privkey.pem",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(conf, "return 301") {
		t.Errorf("force-HTTPS must stay off unless requested\n%s", conf)
	}
	if !strings.Contains(conf, "proxy_pass http://127.0.0.1:3000;") {
		t.Errorf("proxy target missing\n%s", conf)
	}
}

func TestSiteConfExApacheSSL(t *testing.T) {
	conf, err := SiteConfEx("apache", "static", "blog", "blog.example.com", 80, "/var/www/blog", ProxyConf{}, SSLConf{
		Enabled: true, ForceHTTPS: true, Challenge: true,
		CertFile: "/var/lib/lightpanel/acme/certs/blog.example.com/fullchain.pem",
		KeyFile:  "/var/lib/lightpanel/acme/certs/blog.example.com/privkey.pem",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{
		"<VirtualHost *:80>",
		"<VirtualHost *:443>",
		"SSLEngine on",
		"SSLCertificateFile /var/lib/lightpanel/acme/certs/blog.example.com/fullchain.pem",
		"SSLCertificateKeyFile /var/lib/lightpanel/acme/certs/blog.example.com/privkey.pem",
		"Alias /.well-known/acme-challenge/ " + ChallengeDir + "/",
		"RedirectMatch permanent ^/(?!\\.well-known/acme-challenge/)(.*)$ https://blog.example.com/$1",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("conf missing %q\n%s", want, conf)
		}
	}
}

func TestSiteConfExPlainConfUnchanged(t *testing.T) {
	for _, engine := range []string{"nginx", "apache"} {
		conf, err := SiteConfEx(engine, "static", "blog", "blog.example.com", 80, "/var/www/blog", ProxyConf{}, SSLConf{})
		if err != nil {
			t.Fatalf("%s: %v", engine, err)
		}
		if strings.Contains(conf, "443") || strings.Contains(conf, "SSL") || strings.Contains(conf, "ssl") {
			t.Errorf("%s plain conf must stay HTTP-only\n%s", engine, conf)
		}
		if strings.Contains(conf, "acme-challenge") {
			t.Errorf("%s plain conf must not carry the challenge location\n%s", engine, conf)
		}
	}
}

func TestValidPemPath(t *testing.T) {
	valid := []string{
		"/var/lib/lightpanel/acme/certs/blog.example.com/fullchain.pem",
		"/var/lib/lightpanel/acme/certs/blog.example.com/privkey.pem",
		"/root/.local/state/lightpanel/acme/certs/blog.example.com/fullchain.pem",
		"/home/lightpanel/.local/state/lightpanel/acme/certs/blog.example.com/privkey.pem",
	}
	for _, path := range valid {
		if !ValidPemPath(path) {
			t.Errorf("ValidPemPath(%q) = false, want true", path)
		}
	}
	invalid := []string{
		"", "/etc/shadow.pem", "/etc/nginx/cert.pem", "cert.pem",
		"/var/lib/lightpanel/acme/certs/../../etc/shadow.pem",
		"/home/evil/acme/certs/x.pem",
		"/home/lightpanel/other/cert.pem",
		"/var/lib/lightpanel/acme/certs/blog.example.com/chain.crt",
	}
	for _, path := range invalid {
		if ValidPemPath(path) {
			t.Errorf("ValidPemPath(%q) = true, want false", path)
		}
	}
}
