//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
	"lightpanel/config"
	"lightpanel/pkg/sysinfo"
)

func testPanel(t *testing.T, readOnly bool) (http.Handler, *config.Config) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("testing-password-long"), 10)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AdminUser: "admin", PasswordHash: string(hash), PublicOrigin: "http://localhost", ReadOnly: readOnly}
	h, err := panelWithConfig(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h, cfg
}

func panelWithConfig(t *testing.T, cfg *config.Config) (http.Handler, error) {
	t.Helper()
	files, err := sysinfo.NewFiles(sysinfo.MaxUpload)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { files.Close() })
	manager := &sysinfo.Manager{Run: func(ctx context.Context, name string, args ...string) (string, error) {
		return name + " " + strings.Join(args, " "), nil
	}}
	return newHandler(cfg, files, manager)
}
func request(h http.Handler, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost")
	r.Header.Set("X-CSRF-Token", csrf)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func login(t *testing.T, h http.Handler) (*http.Cookie, string) {
	t.Helper()
	w := request(h, "POST", "/login", "username=admin&password=testing-password-long", nil, "")
	if w.Code != 303 {
		t.Fatalf("login %d %s", w.Code, w.Body)
	}
	c := w.Result().Cookies()[0]
	w = request(h, "GET", "/", "", c, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	match := regexp.MustCompile(`name="csrf-token" content="([a-f0-9]+)"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("missing csrf meta")
	}
	return c, match[1]
}
func TestPanelRoutesAndFileWorkflow(t *testing.T) {
	h, _ := testPanel(t, false)
	for _, p := range []string{"/api/metrics", "/api/processes", "/api/services", "/api/files", "/api/logs", "/api/firewall"} {
		if w := request(h, "GET", p, "", nil, ""); w.Code != 401 {
			t.Fatalf("public %s: %d", p, w.Code)
		}
	}
	for _, p := range []string{"/login", "/static/app.js", "/static/app.css"} {
		if w := request(h, "GET", p, "", nil, ""); w.Code != 200 {
			t.Fatalf("asset %s: %d", p, w.Code)
		}
	}
	c, csrf := login(t, h)
	for _, p := range []string{"/api/metrics", "/api/processes?q=go", "/api/services", "/api/files", "/api/logs", "/api/firewall?engine=ufw"} {
		w := request(h, "GET", p, "", c, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body)
		}
		var data any
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
	}
	if w := request(h, "GET", "/api/file/delete?path=.", "", c, csrf); w.Code != 405 {
		t.Fatalf("GET mutation %d", w.Code)
	}
	dir := t.TempDir()
	demo := dir + "/demo.txt"
	if w := request(h, "POST", "/api/file/upload?path="+url.QueryEscape(demo), "hello", c, ""); w.Code != 403 {
		t.Fatal("CSRF absent accepted")
	}
	if w := request(h, "POST", "/api/file/upload?path="+url.QueryEscape(demo), "hello", c, csrf); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(h, "POST", "/api/file/chmod", "path="+url.QueryEscape(demo)+"&mode=640", c, csrf); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := request(h, "GET", "/api/file/download?path="+url.QueryEscape(demo), "", c, "")
	if w.Code != 200 || w.Body.String() != "hello" {
		t.Fatal("download", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("unsafe download")
	}
	if w := request(h, "POST", "/api/file/delete", "path="+url.QueryEscape(demo), c, csrf); w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, p := range []string{"/not-found", "/ws/terminal"} {
		if w := request(h, "GET", p, "", c, ""); w.Code != 404 {
			t.Fatal("unexpected catchall")
		}
	}
	if w := request(h, "POST", "/logout", "", c, csrf); w.Code != 303 {
		t.Fatal("logout")
	}
	if w := request(h, "GET", "/api/metrics", "", c, ""); w.Code != 401 {
		t.Fatal("session not revoked")
	}
}
func TestReadOnlyAndOrigin(t *testing.T) {
	h, _ := testPanel(t, true)
	c, csrf := login(t, h)
	if w := request(h, "POST", "/api/file/delete", "path=x", c, csrf); w.Code != 403 {
		t.Fatal("readonly mutation accepted")
	}
	if w := request(h, "POST", "/logout", "", c, csrf); w.Code != 303 {
		t.Fatal("readonly logout rejected")
	}
	r := httptest.NewRequest("POST", "http://localhost/login", strings.NewReader("username=admin&password=testing-password-long"))
	r.Header.Set("Origin", "http://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin login")
	}
	r = httptest.NewRequest("GET", "http://evil.example/", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("host rebinding")
	}
}

func hostRequest(h http.Handler, method, target, host, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://dummy.local"+target, strings.NewReader(body))
	r.Host = host
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestWildcardHostAccess(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("testing-password-long"), 10)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AdminUser: "admin", PasswordHash: string(hash), Host: "0.0.0.0", Port: 8888, PublicOrigin: "http://0.0.0.0:8888"}
	if !cfg.WildcardOrigin() {
		t.Fatal("wildcard origin not detected")
	}
	h, err := panelWithConfig(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ip := "192.168.1.5:8888"
	if w := hostRequest(h, "GET", "/", ip, "", "", nil); w.Code == 403 {
		t.Fatalf("access via real IP rejected: %d", w.Code)
	}
	if w := hostRequest(h, "POST", "/login", ip, "username=admin&password=testing-password-long", "http://evil.example", nil); w.Code != 403 {
		t.Fatal("cross-origin login accepted")
	}
	w := hostRequest(h, "POST", "/login", ip, "username=admin&password=testing-password-long", "http://"+ip, nil)
	if w.Code != 303 {
		t.Fatalf("login via IP: %d %s", w.Code, w.Body.String())
	}
	c := w.Result().Cookies()[0]
	for _, bad := range []string{"", "evil.com/x", "a@b", "x?y"} {
		if w := hostRequest(h, "GET", "/", bad, "", "", nil); w.Code != 403 {
			t.Fatalf("malformed host %q accepted: %d", bad, w.Code)
		}
	}
	w = hostRequest(h, "GET", "/", ip, "", "", c)
	if w.Code != 200 {
		t.Fatalf("index via IP: %d", w.Code)
	}
	csrf := regexp.MustCompile(`name="csrf-token" content="([a-f0-9]+)"`).FindStringSubmatch(w.Body.String())
	if len(csrf) != 2 {
		t.Fatal("missing csrf meta")
	}
	if w := hostRequest(h, "POST", "/logout", ip, "csrf="+csrf[1], "http://evil.example", c); w.Code != 403 {
		t.Fatal("cross-origin logout accepted")
	}
	if w := hostRequest(h, "POST", "/logout", ip, "csrf="+csrf[1], "http://"+ip, c); w.Code != 303 {
		t.Fatalf("logout via IP: %d %s", w.Code, w.Body.String())
	}
}
func TestTemplateEscaping(t *testing.T) {
	tmpl, err := template.ParseFS(embeddedFiles, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tmpl.ExecuteTemplate(&b, "index.html", map[string]any{"User": "<script>alert(1)</script>", "CSRF": "safe", "UploadMB": 32, "Version": "dev"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "<script>alert(1)</script>") {
		t.Fatal("unescaped username")
	}
}
func TestHTMLDownloadIsAttachment(t *testing.T) {
	// Contract: uploaded HTML remains attachment bytes, not a template response.
	h, _ := testPanel(t, false)
	c, csrf := login(t, h)
	dir := t.TempDir()
	evil := dir + "/evil.html"
	payload := "<script>alert(1)</script>"
	w := request(h, "POST", "/api/file/upload?"+url.Values{"path": {evil}}.Encode(), payload, c, csrf)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	w = request(h, "GET", "/api/file/download?"+url.Values{"path": {evil}}.Encode(), "", c, "")
	b, _ := io.ReadAll(w.Result().Body)
	if string(b) != payload || w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("unsafe attachment response")
	}
}

func TestAuditRecordsTargetWithoutFileContents(t *testing.T) {
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(previous)
	h, _ := testPanel(t, false)
	c, csrf := login(t, h)
	dir := t.TempDir()
	audit := dir + "/audit.txt"
	w := request(h, "POST", "/api/file/upload?"+url.Values{"path": {audit}}.Encode(), "PRIVATE-FILE-CONTENT", c, csrf)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	logs := buf.String()
	if !strings.Contains(logs, `"msg":"audit_end"`) || !strings.Contains(logs, `"path":"`+audit+`"`) {
		t.Fatal("missing operation audit")
	}
	for _, secret := range []string{"PRIVATE-FILE-CONTENT", "testing-password-long", csrf, c.Value} {
		if strings.Contains(logs, secret) {
			t.Fatal("secret leaked into audit")
		}
	}
}
