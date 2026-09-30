//go:build linux

// Package terminal 提供面板的网页终端：与宝塔面板一致，进入终端页即得到
// 一个由 xterm.js 渲染的真实 PTY 会话。会话始终是 root 登录 shell——root
// 面板直接在本地派生，非 root 面板经 lightpanel-helper 中继；访问仅由面板
// 登录会话保护——没有独立票据、连接额度、会话时长或输入输出上限。
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

// Manager 持有全部在跑的终端会话，面板退出时统一回收。
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

// Close 终止所有在跑会话并等待其退出，此后拒绝新会话。
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

// audit 记录生命周期事件（开始/结束/拒绝），不含命令或输出正文。
func (m *Manager) audit(r *http.Request, event, reason string, extra ...any) {
	args := append([]any{"module", "web_terminal", "event", event, "reason", reason, "user", m.cfg.AdminUser, "peer", r.RemoteAddr, "route", r.URL.Path}, extra...)
	slog.Info("terminal_audit", args...)
}

// sameOrigin 校验握手 Origin 与面板服务地址一致。浏览器发起 WebSocket 必带
// Origin，而面板其他路由的表单/CSRF 防护对 WebSocket 不适用，跨站页面不得
// 借登录 cookie 建立连接；非浏览器客户端（curl、wscat）不带 Origin，允许直连。
func sameOrigin(r *http.Request, cfg *config.Config) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	// TLS 可能终止在反向代理。与登录/CSRF 校验一致使用配置的 public_origin，
	// 不信任客户端转发头。
	if cfg.WildcardOrigin() {
		return origin == cfg.WildcardScheme()+"://"+r.Host
	}
	return origin == cfg.PublicOrigin && u.Host == r.Host
}

// Connect 把一个已认证请求升级为 WebSocket，并在其上服务一个交互式 shell。
// 会话在客户端断开、登录会话被撤销或面板关闭时结束——仅此而已。
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
	if !sameOrigin(r, m.cfg) {
		m.audit(r, "denied", "cross_origin")
		http.Error(w, "cross-origin websocket rejected", 403)
		return
	}
	if !m.track() {
		http.Error(w, "server shutting down", 503)
		return
	}
	defer m.untrack()
	// Origin 已在上方校验；放行 Upgrader 的 CheckOrigin，避免无 Origin 的
	// 非浏览器客户端被重复拒绝。
	up := websocket.Upgrader{ReadBufferSize: 8192, WriteBufferSize: 8192, HandshakeTimeout: 10 * time.Second, CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		m.audit(r, "denied", "upgrade_failed")
		return
	}
	defer ws.Close()
	m.run(r, ws, revoked)
}
