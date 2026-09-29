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

// shellCommand builds a login shell with a normal user environment. Only the
// panel's own secrets (LP_PASS_HASH, proxy credentials) are withheld.
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

func (m *Manager) run(r *http.Request, ws *websocket.Conn, revoked <-chan struct{}) {
	start := time.Now()
	cmd := shellCommand()
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		m.audit(r, "start_failed", "pty_start", "err", err.Error())
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "pty_start_failed"), time.Now().Add(time.Second))
		return
	}
	// Re-wrap after O_NONBLOCK so os.File enables the runtime poller.
	fd, err := unix.FcntlInt(tty.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		killSession(cmd.Process.Pid)
		_ = tty.Close()
		_ = cmd.Wait()
		m.audit(r, "start_failed", "pty_dup", "err", err.Error())
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "pty_start_failed"), time.Now().Add(time.Second))
		return
	}
	_ = tty.Close()
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		killSession(cmd.Process.Pid)
		_ = cmd.Wait()
		m.audit(r, "start_failed", "pty_nonblock", "err", err.Error())
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "pty_start_failed"), time.Now().Add(time.Second))
		return
	}
	tty = os.NewFile(uintptr(fd), "terminal-pty")
	m.audit(r, "started", "", "pid", cmd.Process.Pid, "uid", os.Geteuid())
	pid := cmd.Process.Pid

	// Hijacked HTTP deadlines must not impose the panel's request timeout.
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
			// Client vanished; the shell and its jobs are torn down below.
			if reason == "" {
				reason = "client_disconnected"
			}
		case <-exited:
			// Shell finished; give the output pump a moment to flush the
			// final screen (a slow reader cannot hold the slot forever).
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

// Job-control shells use multiple process groups. Reap the entire PTY session,
// pinning each process with pidfd before verifying its session ID. Deliberately
// detached processes can escape a session; this module is not a sandbox.
func killSession(sid int) {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			b, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
			if err == nil {
				end := strings.LastIndexByte(string(b), ')')
				if end >= 0 {
					fields := strings.Fields(string(b[end+1:]))
					if len(fields) > 3 && fields[3] == strconv.Itoa(sid) {
						_ = unix.Kill(pid, unix.SIGKILL)
					}
				}
			}
			continue
		}
		b, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err == nil {
			end := strings.LastIndexByte(string(b), ')')
			if end >= 0 {
				fields := strings.Fields(string(b[end+1:]))
				if len(fields) > 3 && fields[3] == strconv.Itoa(sid) {
					_ = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
				}
			}
		}
		_ = unix.Close(fd)
	}
	_ = unix.Kill(-sid, unix.SIGKILL)
}

// pumpInput streams client keystrokes into the PTY and applies resize
// requests. Unknown control messages are ignored rather than fatal.
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
	// File.Fd would switch the descriptor back to blocking mode.
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
