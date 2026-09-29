//go:build linux

package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The Web Shell authenticates with the panel login session itself: no ticket
// endpoint exists anymore, and the websocket route requires a valid session
// cookie. A cross-origin browser page must not be able to ride the cookie.
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
	h, _ := testPanel(t, false)
	defer h.(*panelHandler).Close()
	c, csrf := login(t, h)
	server := httptest.NewServer(h)
	defer server.Close()
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/terminal"
	header := http.Header{"Origin": {server.URL}, "Cookie": {c.String()}}
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
	if w := request(h, "POST", "/logout", "", c, csrf); w.Code != 303 {
		t.Fatalf("logout %d %s", w.Code, w.Body)
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
