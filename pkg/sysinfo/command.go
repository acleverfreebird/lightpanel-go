package sysinfo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const outputLimit = 1 << 20

var ErrUnavailable = errors.New("required system utility unavailable")

type Runner func(context.Context, string, ...string) (string, error)
type limitedBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := outputLimit - b.Len()
	if len(p) > left {
		p = p[:left]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func executable(name string) (string, error) {
	switch name {
	case "systemctl", "journalctl", "ufw", "firewall-cmd", "nginx", "docker", "apache2ctl", "httpd", "certbot",
		"apt-get", "dnf", "yum", "zypper", "apk",
		// database engines and clients (detection and management)
		"mysql", "mariadb", "mysqld", "mariadbd", "postgres", "psql", "createdb", "dropdb",
		"runuser", "redis-server", "redis-cli":
	default:
		return "", ErrUnavailable
	}
	for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		p := filepath.Join(dir, name)
		if s, e := os.Stat(p); e == nil && s.Mode().IsRegular() && s.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", ErrUnavailable
}
func RunCommand(parent context.Context, name string, args ...string) (string, error) {
	return RunCommandTimeout(parent, 8*time.Second, name, args...)
}

// RunCommandTimeout allows long-running operations such as docker image
// pulls. Every other guarantee is identical to RunCommand: fixed binary
// lookup, scrubbed environment and a hard output cap.
func RunCommandTimeout(parent context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	return runCommand(parent, timeout, "", name, args...)
}

// RunCommandStdin mirrors RunCommandTimeout but feeds stdin to the child.
// Database user operations use it so passwords never appear in argv.
func RunCommandStdin(parent context.Context, timeout time.Duration, stdin, name string, args ...string) (string, error) {
	return runCommand(parent, timeout, stdin, name, args...)
}

func runCommand(parent context.Context, timeout time.Duration, stdin, name string, args ...string) (string, error) {
	p, err := executable(name)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "SYSTEMD_PAGER=cat", "SYSTEMD_COLORS=0"}
	cmd.WaitDelay = time.Second
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var b limitedBuffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	err = cmd.Run()
	if ctx.Err() != nil {
		return b.String(), ctx.Err()
	}
	if b.truncated {
		return b.String(), fmt.Errorf("command output exceeded 1 MiB")
	}
	return b.String(), err
}
func JSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func commandError(w http.ResponseWriter, out string, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, ErrUnavailable) {
		status = 501
	}
	if errors.Is(err, context.DeadlineExceeded) {
		status = 504
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "operation failed: %v\n%s", err, out)
}
func readLimited(path string, n int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, n))
}
