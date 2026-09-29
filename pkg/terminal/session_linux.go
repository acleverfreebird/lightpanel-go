//go:build linux

package terminal

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	"golang.org/x/sys/unix"
)

const maxInput = 1 << 20
const maxOutput = 16 << 20

func (m *Manager) run(r *http.Request, ws *websocket.Conn, expires time.Time, revoked <-chan struct{}) {
	id := token()[:16]
	start := time.Now()
	if err := requirePidfd(); err != nil {
		m.audit(r, "start_failed", "pidfd_unavailable", id)
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "pidfd_unavailable"), time.Now().Add(time.Second))
		return
	}
	cmd := exec.Command("/bin/sh", "-i")
	// Do not expose panel environment (LP_PASS_HASH, proxy credentials, etc.).
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "TERM=dumb", "LANG=C.UTF-8", "HOME=/"}
	cmd.Dir = "/"
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		m.audit(r, "start_failed", "pty_start", id)
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1011, "pty_start_failed"), time.Now().Add(time.Second))
		return
	}
	// Re-wrap after O_NONBLOCK so os.File enables the runtime poller.
	fd, err := unix.FcntlInt(tty.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		killSession(cmd.Process.Pid)
		_ = tty.Close()
		_ = cmd.Wait()
		m.audit(r, "start_failed", "pty_dup", id)
		return
	}
	_ = tty.Close()
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		killSession(cmd.Process.Pid)
		_ = cmd.Wait()
		m.audit(r, "start_failed", "pty_nonblock", id)
		return
	}
	tty = os.NewFile(uintptr(fd), "terminal-pty")
	m.audit(r, "started", "", id, "pid", cmd.Process.Pid, "uid", os.Geteuid())
	ws.SetReadLimit(4096)
	// Hijacked HTTP deadlines must not impose the panel's 60-second timeout.
	_ = ws.SetReadDeadline(time.Time{})
	var in, out atomic.Int64
	var lastInput atomic.Int64
	lastInput.Store(time.Now().UnixNano())
	events := make(chan string, 3)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() { defer workers.Done(); events <- readInput(ws, tty, &in, &lastInput) }()
	go func() { defer workers.Done(); events <- writeOutput(ws, tty, &out) }()
	exited := make(chan struct{})
	go func() {
		// Observe exit without reaping: keep the shell PID/session ID reserved
		// until all job-control groups have been cleaned up.
		var info unix.Siginfo
		for unix.Waitid(unix.P_PID, cmd.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil) == unix.EINTR {
		}
		close(exited)
	}()
	shellExit := (<-chan struct{})(exited)
	var drain <-chan time.Time
	lifetime := m.lifetime
	if remaining := time.Until(expires); remaining < lifetime {
		lifetime = remaining
	}
	absolute := time.NewTimer(lifetime)
	defer absolute.Stop()
	idle := time.NewTimer(m.idle)
	defer idle.Stop()
	reason := ""
	for reason == "" {
		select {
		case reason = <-events:
		case <-shellExit:
			shellExit = nil
			// Drain buffered final output, but never let surviving jobs or a
			// slow reader hold the PTY indefinitely. Revocation stays immediate.
			timer := time.NewTimer(100 * time.Millisecond)
			defer timer.Stop()
			drain = timer.C
		case <-drain:
			reason = "shell_exit"
		case <-revoked:
			reason = "session_revoked"
		case <-m.done:
			reason = "server_shutdown"
		case <-r.Context().Done():
			reason = "request_cancelled"
		case <-absolute.C:
			reason = "session_timeout"
		case <-idle.C:
			remaining := m.idle - time.Since(time.Unix(0, lastInput.Load()))
			if remaining <= 0 {
				reason = "idle_timeout"
			} else {
				idle.Reset(remaining)
			}
		}
	}
	_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, reason), time.Now().Add(200*time.Millisecond))
	_ = ws.Close()
	killSession(cmd.Process.Pid)
	_ = tty.Close()
	<-exited
	_ = cmd.Wait()
	workers.Wait()
	m.audit(r, "ended", reason, id, "duration_ms", time.Since(start).Milliseconds(), "input_bytes", in.Load(), "output_bytes", out.Load())
}

func requirePidfd() error {
	// Cleanup also requires readable process metadata, not just pidfd syscalls.
	if _, err := os.ReadFile("/proc/self/stat"); err != nil {
		return err
	}
	fd, err := unix.PidfdOpen(os.Getpid(), 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return unix.PidfdSendSignal(fd, 0, nil, 0)
}

func readInput(ws *websocket.Conn, tty *os.File, total, lastInput *atomic.Int64) string {
	for {
		kind, b, err := ws.ReadMessage()
		if err != nil {
			return "client_disconnected_or_invalid_frame"
		}
		if total.Add(int64(len(b))) > maxInput {
			return "input_limit"
		}
		switch kind {
		case websocket.BinaryMessage:
			if len(b) == 0 {
				continue
			}
			lastInput.Store(time.Now().UnixNano())
			if _, err = tty.Write(b); err != nil {
				return "pty_write_failed"
			}
		case websocket.TextMessage:
			var size struct {
				Type string `json:"type"`
				Rows uint16 `json:"rows"`
				Cols uint16 `json:"cols"`
			}
			if json.Unmarshal(b, &size) != nil || size.Type != "resize" || size.Rows < 1 || size.Rows > 100 || size.Cols < 1 || size.Cols > 240 {
				return "invalid_resize"
			}
			if resizePTY(tty, size.Rows, size.Cols) != nil {
				return "resize_failed"
			}
		default:
			return "invalid_frame"
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
func writeOutput(ws *websocket.Conn, tty *os.File, total *atomic.Int64) string {
	b := make([]byte, 4096)
	for {
		n, err := tty.Read(b)
		if n > 0 {
			if total.Add(int64(n)) > maxOutput {
				return "output_limit"
			}
			_ = ws.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if ws.WriteMessage(websocket.BinaryMessage, b[:n]) != nil {
				return "client_write_failed"
			}
		}
		if err != nil {
			return "pty_closed"
		}
	}
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
	// The original shell group is also killed if opening a descendant pidfd
	// raced with exit. Admission has already verified pidfd availability.
	_ = unix.Kill(-sid, unix.SIGKILL)
}
