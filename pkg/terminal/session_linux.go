//go:build linux

package terminal

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"
)

// shellCommand builds a login shell with a normal root environment. Only the
// panel's own secrets (LP_PASS_HASH, proxy credentials) are withheld.
func shellCommand() *exec.Cmd {
	shell := "/bin/sh"
	if _, err := os.Stat("/bin/bash"); err == nil {
		shell = "/bin/bash"
	}
	home := "/root"
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		home = "/"
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
	return cmd
}

func (m *Manager) run(r *http.Request, ws *websocket.Conn, revoked <-chan struct{}) {
	start := time.Now()
	cmd := shellCommand()
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		m.audit(r, "start_failed", "pty_start")
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "pty_start_failed"), time.Now().Add(time.Second))
		return
	}
	m.audit(r, "started", "", "pid", cmd.Process.Pid, "uid", os.Geteuid())
	pid := cmd.Process.Pid

	// Hijacked HTTP deadlines must not impose the panel's request timeout.
	_ = ws.SetReadDeadline(time.Time{})
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

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
	// pty.StartWithSize makes the shell a session leader, so this reaches the
	// whole process group — editors, pipelines, background jobs. This module
	// is not a sandbox; deliberately detached processes may survive.
	_ = unix.Kill(-pid, unix.SIGKILL)
	_ = tty.Close()
	_ = ws.Close()
	workers.Wait()
	m.audit(r, "ended", reason, "pid", pid, "duration_ms", time.Since(start).Milliseconds())
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
