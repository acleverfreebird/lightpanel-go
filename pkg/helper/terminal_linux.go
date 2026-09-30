//go:build linux

package helper

import (
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	ptyutil "lightpanel/pkg/ptyutil"
)

// rootShellCommand builds a root login shell for the relayed web terminal —
// the helper process already runs as root, so no privilege transition is
// involved.
func rootShellCommand() *exec.Cmd {
	shell := "/bin/sh"
	if _, err := os.Stat("/bin/bash"); err == nil {
		shell = "/bin/bash"
	}
	cmd := exec.Command(shell, "-l")
	if unix.Access("/root", unix.X_OK) == nil {
		cmd.Dir = "/root"
	} // otherwise stay at /: an exotic deployment without /root access
	cmd.Env = []string{
		"TERM=xterm-256color",
		"HOME=/root",
		"SHELL=" + shell,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LANG=C.UTF-8",
		"USER=root",
		"LOGNAME=root",
	}
	return cmd
}

// terminalRelay serves one OpTerminal connection: it answers the request,
// spawns a root login shell on a fresh PTY and pumps the connection until
// either side ends, then kills the whole session. The shell's lifetime is
// bounded by the panel's connection — no ticket, no quota, no timeout —
// exactly like the panel-local terminal.
func (s *server) terminalRelay(uid int, conn net.Conn) {
	log := s.log.With("module", "terminal_relay", "uid", uid)
	if !s.cfg.AllowTerminal {
		writeRelayResponse(conn, Response{OK: false, Error: "terminal relay requires allow_terminal = true in [helper]"})
		return
	}
	cmd := rootShellCommand()
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		log.Error("terminal_spawn_failed", "error", err.Error())
		writeRelayResponse(conn, Response{OK: false, Error: "cannot spawn the root shell: " + err.Error()})
		return
	}
	pid := cmd.Process.Pid
	start := time.Now()
	if !writeRelayResponse(conn, Response{OK: true}) {
		teardownRelay(pid, tty, conn)
		_ = cmd.Wait()
		return
	}
	// The relayed session lives as long as the panel keeps the connection;
	// drop the request deadline set in handle.
	_ = conn.SetDeadline(time.Time{})
	log.Info("terminal_started", "pid", pid)

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	var once sync.Once
	teardown := func() {
		once.Do(func() { teardownRelay(pid, tty, conn) })
	}
	defer teardown()

	// PTY → panel: raw output bytes, no framing. Ends on shell exit, kill or
	// panel disconnect.
	outputDone := make(chan struct{})
	go func() {
		defer close(outputDone)
		buf := make([]byte, 32<<10)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				if _, werr := conn.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Shell gone: bound the final-output flush window, then reclaim the whole
	// session (mirrors the panel-local terminal).
	go func() {
		<-exited
		select {
		case <-outputDone:
		case <-time.After(2 * time.Second):
		}
		teardown()
	}()

	// panel → PTY: framed input and resize messages.
	for {
		kind, payload, err := ReadRelayFrame(conn)
		if err != nil {
			break
		}
		if kind == FrameInput {
			if _, err = tty.Write(payload); err != nil {
				break
			}
		} else if len(payload) == 4 {
			_ = ptyutil.Resize(tty, binary.BigEndian.Uint16(payload[:2]), binary.BigEndian.Uint16(payload[2:]))
		}
	}
	teardown()
	<-exited
	log.Info("terminal_ended", "pid", pid, "duration_ms", time.Since(start).Milliseconds())
}

// teardownRelay kills the shell's whole session and closes both ends. Safe
// to call repeatedly (the caller wraps it in sync.Once).
func teardownRelay(pid int, tty *os.File, conn net.Conn) {
	ptyutil.KillSession(pid)
	_ = tty.Close()
	_ = conn.Close()
}

// writeRelayResponse answers the terminal request inside the short request
// deadline; after an OK answer the connection turns into the raw relay
// stream.
func writeRelayResponse(conn net.Conn, resp Response) bool {
	resp.Version = ProtocolVersion
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	return json.NewEncoder(conn).Encode(resp) == nil
}
