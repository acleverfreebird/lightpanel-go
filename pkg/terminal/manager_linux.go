//go:build linux

// Package terminal contains the high-risk interactive shell boundary. It never
// delegates shell execution to the privileged helper.
package terminal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"lightpanel/config"
	"lightpanel/pkg/auth"
)

const protocol = "lightpanel-terminal"
const ticketTTL = 30 * time.Second
const maxTickets = 128

type ticket struct {
	owner   string
	expires time.Time
}
type Manager struct {
	cfg            *config.Config
	mu             sync.Mutex
	tickets        map[[32]byte]ticket
	active         int
	users          map[string]int
	closed         bool
	done           chan struct{}
	wg             sync.WaitGroup
	idle, lifetime time.Duration
}

func New(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg, tickets: make(map[[32]byte]ticket), users: make(map[string]int), done: make(chan struct{}), idle: 5 * time.Minute, lifetime: 30 * time.Minute}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		clear(m.tickets)
		close(m.done)
	}
	m.mu.Unlock()
	m.wg.Wait()
}

func token() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (m *Manager) audit(r *http.Request, event, reason, id string, extra ...any) {
	args := []any{"module", "web_terminal", "event", event, "reason", reason, "connection_id", id, "user", m.cfg.AdminUser, "peer", r.RemoteAddr, "route", r.URL.Path}
	slog.Info("terminal_audit", append(args, extra...)...)
}
func (m *Manager) deny(w http.ResponseWriter, r *http.Request, status int, reason string) {
	m.audit(r, "denied", reason, "")
	http.Error(w, reason, status)
}

// Gate deliberately does not inherit the panel's wildcard/Referer fallback.
func (m *Manager) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !m.cfg.TerminalOn() {
			m.deny(w, r, 404, "terminal disabled")
			return
		}
		if m.cfg.ReadOnly {
			m.deny(w, r, 403, "terminal requires write access")
			return
		}
		u, err := url.Parse(m.cfg.PublicOrigin)
		if err != nil || u.Host == "" || m.cfg.WildcardOrigin() || len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != m.cfg.PublicOrigin || r.Host != u.Host {
			m.deny(w, r, 403, "terminal requires an exact configured public_origin and Origin")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *Manager) Ticket(w http.ResponseWriter, r *http.Request) {
	owner, expires, revoked := auth.Identity(r)
	if owner == "" || !time.Now().Before(expires) {
		m.deny(w, r, 401, "authentication required")
		return
	}
	select {
	case <-revoked:
		m.deny(w, r, 401, "session revoked")
		return
	default:
	}
	raw := token()
	now := time.Now()
	m.mu.Lock()
	for k, t := range m.tickets {
		if !now.Before(t.expires) || t.owner == owner {
			delete(m.tickets, k)
		}
	}
	if m.closed || len(m.tickets) >= maxTickets {
		m.mu.Unlock()
		m.deny(w, r, 503, "terminal unavailable")
		return
	}
	m.tickets[sha256.Sum256([]byte(raw))] = ticket{owner: owner, expires: now.Add(ticketTTL)}
	m.mu.Unlock()
	m.audit(r, "ticket_issued", "", "")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ticket": raw, "expires_in": int(ticketTTL / time.Second)})
}

func (m *Manager) consume(raw, owner string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sha256.Sum256([]byte(raw))
	t, ok := m.tickets[key]
	delete(m.tickets, key)
	return !m.closed && ok && len(raw) == 64 && owner != "" && t.owner == owner && time.Now().Before(t.expires)
}
func (m *Manager) reserve(user string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.active >= 4 || m.users[user] >= 2 {
		return false
	}
	m.active++
	m.users[user]++
	m.wg.Add(1)
	return true
}
func (m *Manager) release(user string) {
	m.mu.Lock()
	m.active--
	m.users[user]--
	m.mu.Unlock()
	m.wg.Done()
}

func (m *Manager) Connect(w http.ResponseWriter, r *http.Request) {
	owner, expires, revoked := auth.Identity(r)
	var raw string
	protocols := websocket.Subprotocols(r)
	if len(protocols) == 2 && protocols[0] == protocol && strings.HasPrefix(protocols[1], "lp-ticket.") {
		raw = strings.TrimPrefix(protocols[1], "lp-ticket.")
	}
	if !m.consume(raw, owner) {
		m.deny(w, r, 403, "invalid or expired terminal ticket")
		return
	}
	if !time.Now().Before(expires) {
		m.deny(w, r, 401, "session expired")
		return
	}
	select {
	case <-revoked:
		m.deny(w, r, 401, "session revoked")
		return
	default:
	}
	if !m.reserve(m.cfg.AdminUser) {
		m.deny(w, r, 429, "terminal PTY limit reached")
		return
	}
	defer m.release(m.cfg.AdminUser)
	up := websocket.Upgrader{ReadBufferSize: 4096, WriteBufferSize: 4096, HandshakeTimeout: 5 * time.Second, Subprotocols: []string{protocol}, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == m.cfg.PublicOrigin }}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		m.audit(r, "denied", "upgrade_failed", "")
		return
	}
	defer ws.Close()
	m.run(r, ws, expires, revoked)
}
