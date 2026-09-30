//go:build linux

package terminal

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"

	"lightpanel/pkg/helper"
	ptyutil "lightpanel/pkg/ptyutil"
)

// shellSession 是一条已就绪的终端会话。面板进程自身以 root 运行时为本地
// PTY；非 root 面板经特权 helper 中继 root PTY。两者共享 run() 的泵与生命
// 周期逻辑。
type shellSession interface {
	// output 读取 shell 输出；返回错误即输出结束。
	output(p []byte) (int, error)
	// input 写入键盘输入字节。
	input(p []byte) error
	// resize 调整 PTY 窗口大小。
	resize(rows, cols uint16) error
	// exited 在 shell 退出后关闭。
	exited() <-chan struct{}
	// kill 结束整个会话：本地杀掉整个进程组，中继关闭链路（helper 端回收）。
	kill()
	// release 回收会话残留：等待退出监视并收割子进程。
	release()
	// pid 是 shell 进程号；中继会话面板侧不可见，返回 0。
	pid() int
	// fields 是 started 审计事件的字段。
	fields() []any
}

// start brings up one root shell for the terminal: a root panel spawns the
// PTY locally; an unprivileged panel relays through the privileged helper,
// which owns the root PTY. A panel without a [helper] section (legacy
// unprivileged deployment) keeps the legacy behavior of running the shell as
// the panel's own account — startup already warns about that mode. step
// names the failed setup stage for the audit log.
func (m *Manager) start() (sess shellSession, step string, err error) {
	if os.Geteuid() == 0 || m.cfg.Helper == nil {
		return startLocal()
	}
	s, err := startRelay(m.cfg.Helper.Socket)
	if err != nil {
		return nil, "helper", err
	}
	return s, "", nil
}

// shellCommand 构造一个带常规用户环境的登录 shell，只保留面板自身的机密
// 环境变量（LP_PASS_HASH、代理凭据）不继承。
func shellCommand() *exec.Cmd {
	shell := "/bin/sh"
	if _, err := os.Stat("/bin/bash"); err == nil {
		shell = "/bin/bash"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" || unix.Access(home, unix.X_OK) != nil {
		home = "/"
	}
	username := ""
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	} else if u := os.Getenv("USER"); u != "" {
		username = u
	} else if os.Geteuid() == 0 {
		username = "root"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Dir = home
	cmd.Env = []string{
		"TERM=xterm-256color",
		"HOME=" + home,
		"SHELL=" + shell,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
	}
	if username != "" {
		cmd.Env = append(cmd.Env, "USER="+username, "LOGNAME="+username)
	}
	return cmd
}

// startLocal spawns the login shell as a local PTY of the panel process.
func startLocal() (*localSession, string, error) {
	cmd := shellCommand()
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		return nil, "pty_start", err
	}
	abort := func(step string, err error) (*localSession, string, error) {
		_ = tty.Close()
		ptyutil.KillSession(cmd.Process.Pid)
		_ = cmd.Wait()
		return nil, step, err
	}
	// pty.Start 返回的描述符带 O_NONBLOCK；重新封装以便 Go 运行时 netpoller
	// 高效轮询（先复制 fd、关旧、再设非阻塞）。
	fd, err := unix.FcntlInt(tty.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return abort("pty_dup", err)
	}
	_ = tty.Close()
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		return abort("pty_nonblock", err)
	}
	s := &localSession{cmd: cmd, tty: os.NewFile(uintptr(fd), "terminal-pty"), exitedCh: make(chan struct{})}
	go func() {
		var info unix.Siginfo
		for unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil) == unix.EINTR {
		}
		close(s.exitedCh)
	}()
	return s, "", nil
}

type localSession struct {
	cmd      *exec.Cmd
	tty      *os.File
	exitedCh chan struct{}
}

func (s *localSession) output(p []byte) (int, error)   { return s.tty.Read(p) }
func (s *localSession) input(p []byte) error           { _, err := s.tty.Write(p); return err }
func (s *localSession) resize(rows, cols uint16) error { return ptyutil.Resize(s.tty, rows, cols) }
func (s *localSession) exited() <-chan struct{}        { return s.exitedCh }

func (s *localSession) kill() {
	ptyutil.KillSession(s.cmd.Process.Pid)
	_ = s.tty.Close()
}

func (s *localSession) release() {
	<-s.exitedCh
	_ = s.cmd.Wait()
}

func (s *localSession) pid() int { return s.cmd.Process.Pid }

func (s *localSession) fields() []any {
	return []any{"pid", s.cmd.Process.Pid, "uid", os.Geteuid(), "mode", "local"}
}

// startRelay opens a root PTY through the privileged helper: the helper
// spawns the login shell and pumps it over the unix socket until the shell
// exits or the panel hangs up. Errors carry the helper's reason (ACL, stale
// helper protocol, helper down).
func startRelay(socket string) (*relaySession, error) {
	client := &helper.Client{Socket: socket}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := client.Terminal(ctx)
	if err != nil {
		return nil, err
	}
	return &relaySession{conn: conn, exitedCh: make(chan struct{})}, nil
}

type relaySession struct {
	conn     net.Conn
	exitedCh chan struct{}
	once     sync.Once
}

// flagExited marks the relay stream as over; the only reader is the output
// pump, so no synchronization beyond sync.Once is needed.
func (s *relaySession) flagExited() { s.once.Do(func() { close(s.exitedCh) }) }

func (s *relaySession) output(p []byte) (int, error) {
	n, err := s.conn.Read(p)
	if err != nil {
		s.flagExited()
	}
	return n, err
}

func (s *relaySession) input(p []byte) error { return helper.WriteInputFrame(s.conn, p) }
func (s *relaySession) resize(rows, cols uint16) error {
	return helper.WriteResizeFrame(s.conn, rows, cols)
}
func (s *relaySession) exited() <-chan struct{} { return s.exitedCh }
func (s *relaySession) pid() int                { return 0 }
func (s *relaySession) fields() []any           { return []any{"uid", 0, "mode", "helper"} }

// kill 只需挂断：helper 以连接的存续为会话生命周期，收到 EOF 后回收整个
// root 会话。
func (s *relaySession) kill() { _ = s.conn.Close() }

func (s *relaySession) release() {}

// run 在一条 WebSocket 上服务一个终端会话：输入输出双向直传，会话在客户
// 端断开、登录撤销、面板关闭或 shell 退出时结束并回收。
func (m *Manager) run(r *http.Request, ws *websocket.Conn, revoked <-chan struct{}) {
	start := time.Now()
	sess, step, err := m.start()
	if err != nil {
		m.audit(r, "start_failed", step, "err", err.Error())
		// helper 侧的拒绝原因（ACL、协议过旧、helper 未运行）直接给到终端
		// 页面；本地 PTY 失败保持笼统原因。
		reason := "pty_start_failed"
		if step == "helper" {
			if msg := strings.ReplaceAll(err.Error(), "\n", " "); msg != "" && len(msg) <= 120 {
				reason = msg
			}
		}
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, reason), time.Now().Add(time.Second))
		return
	}
	m.audit(r, "started", "", sess.fields()...)

	// HTTP 劫持后的连接不受面板请求超时约束，清掉升级期限。
	_ = ws.SetReadDeadline(time.Time{})
	exited := sess.exited()

	clientGone := make(chan string, 1)
	outputDone := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		clientGone <- pumpInput(ws, sess)
	}()
	go func() {
		defer workers.Done()
		defer close(outputDone)
		pumpOutput(ws, sess)
	}()

	reason := ""
	for reason == "" {
		select {
		case reason = <-clientGone:
			// 客户端消失；会话在下方整体回收。
			if reason == "" {
				reason = "client_disconnected"
			}
		case <-exited:
			// shell 已退出；给输出泵一点时间冲刷最后一屏（慢读者不可能
			// 长期占住会话）。
			select {
			case <-outputDone:
			case <-time.After(2 * time.Second):
			}
			reason = "shell_exit"
		case <-revoked:
			reason = "session_revoked"
		case <-m.done:
			reason = "server_shutdown"
		}
	}
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, reason), time.Now().Add(time.Second))
	sess.kill()
	_ = ws.Close()
	sess.release()
	workers.Wait()
	m.audit(r, "ended", reason, "pid", sess.pid(), "duration_ms", time.Since(start).Milliseconds())
}

// pumpInput 把客户端按键流入会话并处理 resize 控制帧；未知控制帧忽略，
// 不视为致命错误。
func pumpInput(ws *websocket.Conn, sess shellSession) string {
	for {
		kind, b, err := ws.ReadMessage()
		if err != nil {
			return "client_disconnected"
		}
		switch kind {
		case websocket.BinaryMessage:
			if len(b) > 0 {
				if err = sess.input(b); err != nil {
					return "pty_write_failed"
				}
			}
		case websocket.TextMessage:
			var msg struct {
				Type string `json:"type"`
				Rows uint16 `json:"rows"`
				Cols uint16 `json:"cols"`
			}
			if json.Unmarshal(b, &msg) != nil || msg.Type != "resize" {
				continue
			}
			if msg.Rows < 1 || msg.Rows > 1000 || msg.Cols < 1 || msg.Cols > 1000 {
				continue
			}
			_ = sess.resize(msg.Rows, msg.Cols)
		}
	}
}

// pumpOutput 把 shell 输出流入客户端。
func pumpOutput(ws *websocket.Conn, sess shellSession) {
	b := make([]byte, 32<<10)
	for {
		n, err := sess.output(b)
		if n > 0 {
			_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if ws.WriteMessage(websocket.BinaryMessage, b[:n]) != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
