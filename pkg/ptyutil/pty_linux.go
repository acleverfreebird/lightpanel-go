//go:build linux

// Package ptyutil holds the PTY session primitives shared by the panel's local
// terminal and the privileged helper's relayed terminal.
package ptyutil

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// KillSession tears down a whole PTY session: job-control shells use several
// process groups, so every process whose session ID matches is signaled.
// Each process is pinned with pidfd before its /proc session ID is checked,
// closing the PID-reuse race between reading /proc and signaling. Processes
// that deliberately setsid away are out of scope; this is not a sandbox.
func KillSession(sid int) {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if err != nil {
			// Fall back to plain signals on kernels without pidfd.
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

// Resize sets the PTY window size.
func Resize(f *os.File, rows, cols uint16) error {
	// File.Fd would switch the descriptor back to blocking mode; go through
	// SyscallConn.
	raw, err := f.SyscallConn()
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

// sessionMatch reports whether the process behind /proc/<pid>/stat belongs to
// the given session.
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
