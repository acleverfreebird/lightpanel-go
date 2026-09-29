//go:build linux

// Package terminal serves the panel's Web Shell: a full interactive PTY
// streamed over a WebSocket and rendered by xterm.js in the frontend, in the
// spirit of BT Panel's terminal. The shell runs as the panel service account
// and is gated only by the panel login session — there are no tickets,
// session quotas, timeouts or input/output caps.
package terminal

import (
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"lightpanel/config"
	"lightpanel/pkg/auth"
)

type Manager struct {
	cfg    *config.Config
	mu     sync.Mutex
	closed bool
	done   chan struct{}
	wg     sync.WaitGroup
}

func New(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg, done: make(chan struct{})}
}

// Close terminates every live shell session and waits for them to unwind.
// New sessions are rejected afterwards.
func (m *Manager) Close() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		close(m.done)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *Manager) track() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.wg.Add(1)
	return true
}

func (m *Manager) untrack() { m.wg.Done() }

func (m *Manager) audit(r *http.Request, event, reason string, extra ...any) {
	args := append([]any{"module", "web_terminal", "event", event, "reason", reason, "user", m.cfg.AdminUser, "peer", r.RemoteAddr, "route", r.URL.Path}, extra...)
	slog.Info("terminal_audit", args...)
}

// sameOrigin accepts handshakes whose Origin matches the served host.
// Browsers always send Origin on WebSocket handshakes, and the form/CSRF
// protections used by the panel's other routes do not apply to WebSocket, so
// a cross-site page must not be able to ride the session cookie. Non-browser
// clients (curl, wscat) send no Origin and are allowed.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && u.Host == r.Host
}

// Connect upgrades one authenticated request to a WebSocket and serves an
// interactive shell on it. The session ends when the client disconnects, the
// login session is revoked, or the panel shuts down — nothing else.
func (m *Manager) Connect(w http.ResponseWriter, r *http.Request) {
	owner, expires, revoked := auth.Identity(r)
	if owner == "" || !time.Now().Before(expires) {
		http.Error(w, "authentication required", 401)
		return
	}
	select {
	case <-revoked:
		http.Error(w, "session revoked", 401)
		return
	default:
	}
	if !m.cfg.TerminalOn() {
		m.audit(r, "denied", "terminal_disabled")
		http.Error(w, "terminal disabled", 404)
		return
	}
	if m.cfg.ReadOnly {
		m.audit(r, "denied", "read_only")
		http.Error(w, "terminal requires write access", 403)
		return
	}
	if !sameOrigin(r) {
		m.audit(r, "denied", "cross_origin")
		http.Error(w, "cross-origin websocket rejected", 403)
		return
	}
	if !m.track() {
		http.Error(w, "server shutting down", 503)
		return
	}
	defer m.untrack()
	// Origin is checked above; the upgrader's CheckOrigin is bypassed so
	// missing Origin (non-browser clients) is not rejected twice.
	up := websocket.Upgrader{ReadBufferSize: 8192, WriteBufferSize: 8192, HandshakeTimeout: 10 * time.Second, CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		m.audit(r, "denied", "upgrade_failed")
		return
	}
	defer ws.Close()
	m.run(r, ws, revoked)
}
