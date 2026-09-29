//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"github.com/gorilla/websocket"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTerminalAdmission(t *testing.T) {
	h, _ := testPanel(t, false)
	c, csrf := login(t, h)
	if w := request(h, "POST", "/api/terminal/ticket", "", c, csrf); w.Code != 200 {
		t.Fatalf("ticket: %d %s", w.Code, w.Body)
	}
	if w := request(h, "GET", "/ws/terminal", "", c, ""); w.Code != 403 {
		t.Fatalf("missing ticket: %d", w.Code)
	}
	for _, origin := range []string{"", "null", "http://evil.example", "http://localhost/"} {
		r := httptest.NewRequest("POST", "http://localhost/api/terminal/ticket", nil)
		r.AddCookie(c)
		r.Header.Set("Origin", origin)
		r.Header.Set("Referer", "http://localhost/")
		r.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("origin %q accepted: %d", origin, w.Code)
		}
	}
}

func TestTerminalWebSocketTicketLogoutAndAudit(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	h, _ := testPanel(t, false)
	defer h.(*panelHandler).Close()
	c, csrf := login(t, h)
	issue := func() string {
		t.Helper()
		w := request(h, "POST", "/api/terminal/ticket", "", c, csrf)
		var data struct {
			Ticket string `json:"ticket"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &data) != nil || len(data.Ticket) != 64 {
			t.Fatalf("ticket %d %s", w.Code, w.Body)
		}
		return data.Ticket
	}
	if w := request(h, "POST", "/api/terminal/ticket", "", c, ""); w.Code != 403 {
		t.Fatal("CSRF bypass")
	}
	if w := request(h, "GET", "/ws/terminal", "", nil, ""); w.Code != 401 {
		t.Fatal("authentication bypass")
	}
	server := httptest.NewServer(h)
	defer server.Close()
	dial := func(ticket string) (*websocket.Conn, *http.Response, error) {
		d := websocket.Dialer{Subprotocols: []string{"lightpanel-terminal", "lp-ticket." + ticket}}
		return d.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws/terminal", http.Header{"Origin": {"http://localhost"}, "Host": {"localhost"}, "Cookie": {c.String()}})
	}
	ticket := issue()
	ws, resp, err := dial(ticket)
	if err != nil {
		t.Fatalf("upgrade: %v %+v", err, resp)
	}
	defer ws.Close()
	if ws.Subprotocol() != "lightpanel-terminal" {
		t.Fatal("ticket reflected in selected protocol")
	}
	_, resp, err = dial(ticket)
	if err == nil || resp.StatusCode != 403 {
		t.Fatal("ticket replay accepted")
	}
	resp.Body.Close()
	second := issue()
	ws2, _, err := dial(second)
	if err != nil {
		t.Fatal(err)
	}
	defer ws2.Close()
	_, resp, err = dial(issue())
	if err == nil || resp.StatusCode != 429 {
		t.Fatal("account quota bypass")
	}
	resp.Body.Close()
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("printf SECRET_TERMINAL_BODY\r")); err != nil {
		t.Fatal(err)
	}
	if w := request(h, "POST", "/logout", "", c, csrf); w.Code != 303 {
		t.Fatal("logout failed")
	}
	for _, conn := range []*websocket.Conn{ws, ws2} {
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				e, ok := err.(*websocket.CloseError)
				if !ok || e.Text != "session_revoked" {
					t.Fatalf("logout close: %v", err)
				}
				break
			}
		}
	}
	h.(*panelHandler).Close()
	text := logs.String()
	for _, secret := range []string{ticket, second, c.Value, csrf, "SECRET_TERMINAL_BODY", "testing-password-long"} {
		if strings.Contains(text, secret) {
			t.Fatal("audit leaked secret")
		}
	}
	for _, event := range []string{`"event":"ticket_issued"`, `"event":"started"`, `"event":"ended"`, `"reason":"session_revoked"`, `"reason":"terminal PTY limit reached"`} {
		if !strings.Contains(text, event) {
			t.Fatalf("missing audit %s", event)
		}
	}
}

func TestTerminalReadOnly(t *testing.T) {
	h, _ := testPanel(t, true)
	c, csrf := login(t, h)
	for _, route := range []struct{ method, path string }{{"POST", "/api/terminal/ticket"}, {"GET", "/ws/terminal"}} {
		if w := request(h, route.method, route.path, "", c, csrf); w.Code != 403 {
			t.Fatalf("readonly %s: %d", route.path, w.Code)
		}
	}
}
