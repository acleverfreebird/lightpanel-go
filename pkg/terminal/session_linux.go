//go:build linux

package terminal

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"
)

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

// run 在一条 WebSocket 上服务一个 PTY 会话：输入输出双向直传，会话在客户
// 端断开、登录撤销、面板关闭或 shell 退出时结束并清理整个进程组。
func (m *Manager) run(r *http.Request, ws *websocket.Conn, revoked <-chan struct{}) {
	start := time.Now()
	cmd := shellCommand()
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		m.audit(r, "start_failed", "pty_start", "err", err.Error())
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "pty_start_failed"), time.Now().Add(time.Second))
		return
	}
	// pty.Start 返回的描述符带 O_NONBLOCK；重新封装以便 Go 运行时 netpoller
	// 高效轮询（先复制 fd、关旧、再设非阻塞）。
	fd, err := unix.FcntlInt(tty.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		m.cleanup(cmd, tty)
		m.audit(r, "start_failed", "pty_dup", "err", err.Error())
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "pty_start_failed"), time.Now().Add(time.Second))
		return
	}
	_ = tty.Close()
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		m.cleanup(cmd, nil)
		m.audit(r, "start_failed", "pty_nonblock", "err", err.Error())
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "pty_start_failed"), time.Now().Add(time.Second))
		return
	}
	tty = os.NewFile(uintptr(fd), "terminal-pty")
	m.audit(r, "started", "", "pid", cmd.Process.Pid, "uid", os.Geteuid())
	pid := cmd.Process.Pid

	// HTTP 劫持后的连接不受面板请求超时约束，清掉升级期限。
	_ = ws.SetReadDeadline(time.Time{})
	exited := make(chan struct{})
	go func() {
		var info unix.Siginfo
		for unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil) == unix.EINTR {
		}
		close(exited)
	}()

	clientGone := make(chan string, 1)
	outputDone := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		clientGone <- pumpInput(ws, tty)
	}()
	go func() {
		defer workers.Done()
		defer close(outputDone)
		pumpOutput(ws, tty)
	}()

	reason := ""
	for reason == "" {
		select {
		case reason = <-clientGone:
			// 客户端消失；shell 及其作业在下方整体回收。
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
	killSession(pid)
	_ = tty.Close()
	_ = ws.Close()
	<-exited
	_ = cmd.Wait()
	workers.Wait()
	m.audit(r, "ended", reason, "pid", pid, "duration_ms", time.Since(start).Milliseconds())
}

// cleanup 回收启动早期失败的 PTY 子进程。
func (m *Manager) cleanup(cmd *exec.Cmd, tty *os.File) {
	if tty != nil {
		_ = tty.Close()
	}
	if cmd.Process != nil {
		killSession(cmd.Process.Pid)
		_ = cmd.Wait()
	}
}

// Job-control shell 会使用多个进程组。回收整个 PTY 会话：先用 pidfd 钉住
// 每个进程再核对其会话 ID，避免读取 /proc 与发信号之间 PID 复用误伤。
// 有意 setsid 脱离会话的进程不在此列；本模块不是沙箱。
func killSession(sid int) {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			// 系统过旧不支持 pidfd 时退回普通信号。
			if sessionMatch(entry.Name(), sid) {
				_ = unix.Kill(pid, unix.SIGKILL)
			}
			continue
		}
		if sessionMatch(entry.Name(), sid) {
			_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
		}
		_ = unix.Close(fd)
	}
	_ = unix.Kill(-sid, unix.SIGKILL)
}

// sessionMatch 判断 /proc/<pid>/stat 对应的进程是否属于指定会话。
func sessionMatch(pid string, sid int) bool {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return false
	}
	fields := strings.Fields(string(b[end+1:]))
	return len(fields) > 3 && fields[3] == strconv.Itoa(sid)
}

// pumpInput 把客户端按键流入 PTY 并处理 resize 控制帧；未知控制帧忽略，
// 不视为致命错误。
func pumpInput(ws *websocket.Conn, tty *os.File) string {
	for {
		kind, b, err := ws.ReadMessage()
		if err != nil {
			return "client_disconnected"
		}
		switch kind {
		case websocket.BinaryMessage:
			if len(b) > 0 {
				if _, err = tty.Write(b); err != nil {
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
			_ = resizePTY(tty, msg.Rows, msg.Cols)
		}
	}
}

func resizePTY(tty *os.File, rows, cols uint16) error {
	// File.Fd 会把描述符切回阻塞模式，必须走 SyscallConn。
	raw, err := tty.SyscallConn()
	if err != nil {
		return err
	}
	var resizeErr error
	err = raw.Control(func(fd uintptr) {
		resizeErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
	if err != nil {
		return err
	}
	return resizeErr
}

func pumpOutput(ws *websocket.Conn, tty *os.File) {
	b := make([]byte, 32<<10)
	for {
		n, err := tty.Read(b)
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
