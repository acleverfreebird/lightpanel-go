package auth

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
	"lightpanel/config"
)

func TestSessionCSRFAndRevocation(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("test-password-long"), 10)
	a := New(&config.Config{AdminUser: "admin", PasswordHash: string(hash), PublicOrigin: "http://localhost"})
	tmpl := template.Must(template.New("login.html").Parse(`login`))
	r := httptest.NewRequest("POST", "http://localhost/login", strings.NewReader("username=admin&password=test-password-long"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "http://localhost")
	w := httptest.NewRecorder()
	a.Login(tmpl)(w, r)
	if w.Code != 303 {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("no session")
	}
	cookie := cookies[0]
	protected := a.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(CSRF(r))) }))
	get := httptest.NewRequest("GET", "http://localhost/api/test", nil)
	get.AddCookie(cookie)
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, get)
	csrf := w.Body.String()
	if w.Code != 200 || len(csrf) < 32 {
		t.Fatal("session invalid")
	}
	post := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "http://localhost/api/test", strings.NewReader(url.Values{"csrf": {token}}.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://localhost")
		r.Header.Set("X-CSRF-Token", token)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, r)
		return w
	}
	if post("").Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	if post(csrf).Code != 200 {
		t.Fatal("valid CSRF rejected")
	}
	r = httptest.NewRequest("POST", "http://localhost/logout", nil)
	r.AddCookie(cookie)
	r.Header.Set("X-CSRF-Token", csrf)
	r.Header.Set("Origin", "http://localhost")
	w = httptest.NewRecorder()
	a.Require(http.HandlerFunc(a.Logout)).ServeHTTP(w, r)
	w = httptest.NewRecorder()
	protected.ServeHTTP(w, get)
	if w.Code != 401 {
		t.Fatal("logged-out session still valid")
	}
}

func TestLoginLimitUsesIPNotSourcePort(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("correct-password"), 10)
	a := New(&config.Config{AdminUser: "admin", PasswordHash: string(hash), PublicOrigin: "http://localhost"})
	tmpl := template.Must(template.New("login.html").Parse("login"))
	for i := 0; i < 6; i++ {
		r := httptest.NewRequest("POST", "http://localhost/login", strings.NewReader("username=admin&password=wrong"))
		r.RemoteAddr = "192.0.2.1:" + strconv.Itoa(1000+i)
		r.Header.Set("Origin", "http://localhost")
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		a.Login(tmpl)(w, r)
		want := 401
		if i == 5 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
}
func TestExpiredSessionAndSecureCookie(t *testing.T) {
	a := New(&config.Config{PublicOrigin: "https://localhost"})
	a.sessions["expired"] = session{CSRF: "x", Expires: time.Now().Add(-time.Second)}
	r := httptest.NewRequest("GET", "https://localhost/api/test", nil)
	r.AddCookie(&http.Cookie{Name: "lp_session", Value: "expired"})
	w := httptest.NewRecorder()
	a.Require(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("expired session allowed") })).ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
	c := a.cookie("x", 1)
	if !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure cookie")
	}
}
