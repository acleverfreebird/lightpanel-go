package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"lightpanel/config"
)

type session struct {
	CSRF    string
	Expires time.Time
}
type attempt struct {
	Count int
	Until time.Time
}
type contextKey struct{}
type Auth struct {
	cfg       *config.Config
	mu        sync.Mutex
	sessions  map[string]session
	attempts  map[string]attempt
	loginSlot chan struct{}
}

func New(c *config.Config) *Auth {
	return &Auth{cfg: c, sessions: map[string]session{}, attempts: map[string]attempt{}, loginSlot: make(chan struct{}, 1)}
}
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func CSRF(r *http.Request) string { s, _ := r.Context().Value(contextKey{}).(session); return s.CSRF }
func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}
func (a *Auth) sameOrigin(r *http.Request) bool {
	if r.Host != mustHost(a.cfg.PublicOrigin) {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		return o == a.cfg.PublicOrigin
	}
	u, err := url.Parse(r.Header.Get("Referer"))
	return err == nil && u.Scheme+"://"+u.Host == a.cfg.PublicOrigin
}
func mustHost(origin string) string {
	u, _ := url.Parse(origin)
	if u == nil {
		return ""
	}
	return u.Host
}
func (a *Auth) cleanLocked(now time.Time) {
	for k, s := range a.sessions {
		if !now.Before(s.Expires) {
			delete(a.sessions, k)
		}
	}
	for k, v := range a.attempts {
		if !now.Before(v.Until) {
			delete(a.attempts, k)
		}
	}
}
func (a *Auth) cookie(value string, age int) *http.Cookie {
	return &http.Cookie{Name: "lp_session", Value: value, Path: "/", HttpOnly: true, Secure: strings.HasPrefix(a.cfg.PublicOrigin, "https://"), SameSite: http.SameSiteStrictMode, MaxAge: age}
}
func (a *Auth) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("lp_session")
		var s session
		var ok bool
		if err == nil {
			a.mu.Lock()
			s, ok = a.sessions[c.Value]
			if ok && !time.Now().Before(s.Expires) {
				delete(a.sessions, c.Value)
				ok = false
			}
			a.mu.Unlock()
		}
		if !ok {
			if r.URL.Path == "/" {
				http.Redirect(w, r, "/login", 303)
			} else {
				http.Error(w, "authentication required", 401)
			}
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if a.cfg.ReadOnly && r.URL.Path != "/logout" {
				http.Error(w, "read-only account", 403)
				return
			}
			token := r.Header.Get("X-CSRF-Token")
			if token == "" && r.URL.Path == "/logout" {
				token = r.FormValue("csrf")
			}
			if !a.sameOrigin(r) || subtle.ConstantTimeCompare([]byte(token), []byte(s.CSRF)) != 1 {
				http.Error(w, "CSRF check failed", 403)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), contextKey{}, s)))
	})
}
func (a *Auth) Login(t *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			if err := t.ExecuteTemplate(w, "login.html", nil); err != nil {
				slog.Error("template", "error", err)
			}
			return
		}
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		if !a.sameOrigin(r) {
			http.Error(w, "origin rejected", 403)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid login form", 400)
			return
		}
		ip := clientIP(r)
		now := time.Now()
		a.mu.Lock()
		a.cleanLocked(now)
		v := a.attempts[ip]
		blocked := v.Count >= 5 || len(a.attempts) >= 1024 || len(a.sessions) >= 128
		if !blocked {
			v.Count++
			v.Until = now.Add(15 * time.Minute)
			a.attempts[ip] = v
		}
		a.mu.Unlock()
		if blocked {
			http.Error(w, "too many attempts; retry later", 429)
			return
		}
		select {
		case a.loginSlot <- struct{}{}:
			defer func() { <-a.loginSlot }()
		default:
			http.Error(w, "login busy", 429)
			return
		}
		passOK := bcrypt.CompareHashAndPassword([]byte(a.cfg.PasswordHash), []byte(r.Form.Get("password"))) == nil
		if !passOK || r.Form.Get("username") != a.cfg.AdminUser {
			slog.Warn("login", "ip", ip, "success", false)
			http.Error(w, "invalid credentials", 401)
			return
		}
		id := randomToken()
		s := session{CSRF: randomToken(), Expires: now.Add(8 * time.Hour)}
		a.mu.Lock()
		delete(a.attempts, ip)
		if c, e := r.Cookie("lp_session"); e == nil {
			delete(a.sessions, c.Value)
		}
		a.sessions[id] = s
		a.mu.Unlock()
		http.SetCookie(w, a.cookie(id, 8*3600))
		slog.Info("login", "user", a.cfg.AdminUser, "ip", ip, "success", true)
		http.Redirect(w, r, "/", 303)
	}
}
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("lp_session"); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, a.cookie("", -1))
	http.Redirect(w, r, "/login", 303)
}
