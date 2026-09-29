//go:build linux

package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"
	"lightpanel/config"
)

// The web terminal authenticates with the panel login session itself: no
// ticket endpoint exists anymore, and the websocket route requires a valid
// session cookie. A cross-origin browser page must not be able to ride the
// cookie.
func TestTerminalAdmission(t *testing.T) {
	h, _ := testPanel(t, false)
	c, csrf := login(t, h)
	if w := request(h, "POST", "/api/terminal/ticket", "", c, csrf); w.Code != 404 && w.Code != 405 {
		t.Fatalf("ticket endpoint should be gone: %d", w.Code)
	}
	if w := request(h, "GET", "/ws/terminal", "", nil, ""); w.Code != 401 {
		t.Fatalf("unauthenticated websocket: %d", w.Code)
	}
	// Authenticated, no Origin (non-browser client): passes admission and
	// fails only at the websocket handshake.
	if w := request(h, "GET", "/ws/terminal", "", c, ""); w.Code != 400 {
		t.Fatalf("authenticated non-upgrade request: %d", w.Code)
	}
	for _, origin := range []string{"http://evil.example", "http://localhost:81", "null"} {
		r := httptest.NewRequest("GET", "http://localhost/ws/terminal", nil)
		r.AddCookie(c)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("origin %q accepted: %d", origin, w.Code)
		}
	}
}

func TestTerminalWebSocketAndRevocation(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	hash, _ := bcrypt.GenerateFromPassword([]byte("testing-password-long"), 10)
	cfg := &config.Config{AdminUser: "admin", PasswordHash: string(hash), Host: "0.0.0.0", Port: 8888, PublicOrigin: "http://0.0.0.0:8888"}
	h, err := panelWithConfig(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer h.(*panelHandler).Close()
	server := httptest.NewServer(h)
	defer server.Close()
	serverHost := strings.TrimPrefix(server.URL, "http://")
	// Login via the real server Host so the security middleware accepts it.
	loginReq := httptest.NewRequest("POST", server.URL+"/login", strings.NewReader("username=admin&password=testing-password-long"))
	loginReq.Host = serverHost
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginReq.Header.Set("Origin", "http://"+serverHost)
	loginW := httptest.NewRecorder()
	h.ServeHTTP(loginW, loginReq)
	if loginW.Code != 303 {
		t.Fatalf("login %d %s", loginW.Code, loginW.Body)
	}
	c := loginW.Result().Cookies()[0]
	// Retrieve CSRF token from the index page.
	indexReq := httptest.NewRequest("GET", server.URL+"/", nil)
	indexReq.Host = serverHost
	indexReq.AddCookie(c)
	indexW := httptest.NewRecorder()
	h.ServeHTTP(indexW, indexReq)
	if indexW.Code != 200 {
		t.Fatal(indexW.Code)
	}
	csrfMatch := regexp.MustCompile(`name="csrf-token" content="([a-f0-9]+)"`).FindStringSubmatch(indexW.Body.String())
	if len(csrfMatch) != 2 {
		t.Fatal("missing csrf meta")
	}
	csrf := csrfMatch[1]

	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/terminal"
	header := http.Header{"Origin": {"http://" + serverHost}, "Cookie": {c.String()}}
	ws, resp, err := d.Dial(url, header)
	if err != nil {
		t.Fatalf("upgrade: %v %+v", err, resp)
	}
	defer ws.Close()
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("printf 'SECRET_TERMINAL_BODY'\r")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for !strings.Contains(output.String(), "SECRET_TERMINAL_BODY") {
		_, b, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("output so far %q: %v", output.String(), err)
		}
		output.Write(b)
	}
	// Logging out revokes the login session the shell runs on, closing the
	// live websocket with the revocation reason.
	logoutReq := httptest.NewRequest("POST", server.URL+"/logout", strings.NewReader(""))
	logoutReq.Host = serverHost
	logoutReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	logoutReq.Header.Set("Origin", "http://"+serverHost)
	logoutReq.Header.Set("X-CSRF-Token", csrf)
	logoutReq.AddCookie(c)
	logoutW := httptest.NewRecorder()
	h.ServeHTTP(logoutW, logoutReq)
	if logoutW.Code != 303 {
		t.Fatalf("logout %d %s", logoutW.Code, logoutW.Body)
	}
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			if e, ok := err.(*websocket.CloseError); !ok || e.Text != "session_revoked" {
				t.Fatalf("logout close: %v", err)
			}
			break
		}
	}
	if _, resp, err := d.Dial(url, header); err == nil || resp.StatusCode != 401 {
		t.Fatalf("revoked cookie still connects: %v %d", err, resp.StatusCode)
	}
	h.(*panelHandler).Close()
	text := logs.String()
	for _, secret := range []string{"SECRET_TERMINAL_BODY", "testing-password-long"} {
		if strings.Contains(text, secret) {
			t.Fatal("audit leaked secret")
		}
	}
	for _, event := range []string{`"event":"started"`, `"event":"ended"`, `"reason":"session_revoked"`} {
		if !strings.Contains(text, event) {
			t.Fatalf("missing audit %s", event)
		}
	}
}

func TestTerminalReadOnly(t *testing.T) {
	h, _ := testPanel(t, true)
	c, _ := login(t, h)
	r := httptest.NewRequest("GET", "http://localhost/ws/terminal", nil)
	r.Header.Set("Origin", "http://localhost")
	r.AddCookie(c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("readonly websocket: %d", w.Code)
	}
}

func TestTerminalTLSProxyAdmission(t *testing.T) {
	for _, publicOrigin := range []string{"https://panel.example", "https://0.0.0.0:8888"} {
		t.Run(publicOrigin, func(t *testing.T) {
			hash, err := bcrypt.GenerateFromPassword([]byte("testing-password-long"), 10)
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{AdminUser: "admin", PasswordHash: string(hash), PublicOrigin: publicOrigin}
			h, err := panelWithConfig(t, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer h.(*panelHandler).Close()
			// Login and upgrade both arrive over HTTP after TLS termination.
			loginReq := httptest.NewRequest("POST", "http://panel.example/login", strings.NewReader("username=admin&password=testing-password-long"))
			loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			loginReq.Header.Set("Origin", "https://panel.example")
			loginW := httptest.NewRecorder()
			h.ServeHTTP(loginW, loginReq)
			if loginW.Code != 303 {
				t.Fatalf("login: %d", loginW.Code)
			}
			c := loginW.Result().Cookies()[0]
			for _, tc := range []struct {
				origin string
				want   int
			}{
				{"https://panel.example", 400}, // Accepted, only upgrade headers missing.
				{"http://panel.example", 403},
				{"https://evil.example", 403},
				{"https://panel.example:444", 403},
				{"https://user@panel.example", 403},
				{"https://panel.example?query", 403},
			} {
				r := httptest.NewRequest("GET", "http://panel.example/ws/terminal", nil)
				r.AddCookie(c)
				r.Header.Set("Origin", tc.origin)
				r.Header.Set("X-Forwarded-Proto", "https")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.want {
					t.Errorf("origin %q: got %d want %d", tc.origin, w.Code, tc.want)
				}
			}
		})
	}
}
