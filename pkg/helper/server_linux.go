//go:build linux

package helper

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ServerConfig is the helper-side view of the [helper] config section.
type ServerConfig struct {
	Socket        string
	AllowedUsers  []string
	Services      map[string][]string
	AllowFirewall bool
	AllowKill     bool
	AllowUpdate   bool
	// AllowSites gates the managed web-site operations (configuration write,
	// managed delete, engine reload, certbot). These are name-parameterized,
	// so unlike services they are a single opt-in grant.
	AllowSites bool
	// AllowApps gates app-store installs through the system package manager.
	// App names and every argument are rebuilt helper-side from a fixed
	// catalog, so this is also a single opt-in grant.
	AllowApps bool
	// AllowDatabases gates managed database operations (list, create/drop
	// database, user management). Engines, names and every argument are
	// rebuilt helper-side from databases.go, so this is a single opt-in grant.
	AllowDatabases bool
	// PanelUnit is the systemd unit restarted after a successful self-update.
	PanelUnit string
}

const (
	execTimeout  = 15 * time.Second
	updateExeMax = 64 << 20
)

// Drain command output without allowing a privileged command to exhaust memory.
type commandBuffer struct {
	bytes.Buffer
	truncated bool
}

func (b *commandBuffer) Write(p []byte) (int, error) {
	n := len(p)
	left := (1 << 20) - b.Len()
	if len(p) > left {
		p = p[:left]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

// ResolveUsers maps configured user names to UIDs (with the primary GID of
// the first user for socket ownership).
func ResolveUsers(names []string) (uids []int, gid int, err error) {
	for _, name := range names {
		if !ValidUserName(name) {
			return nil, 0, fmt.Errorf("helper.allowed_users: invalid user name %q", name)
		}
		u, e := user.Lookup(name)
		if e != nil {
			return nil, 0, fmt.Errorf("helper.allowed_users: cannot resolve user %q: %v", name, e)
		}
		id, e := strconv.Atoi(u.Uid)
		if e != nil {
			return nil, 0, fmt.Errorf("helper.allowed_users: user %q has invalid uid", name)
		}
		uids = append(uids, id)
		if gid == 0 {
			gid, _ = strconv.Atoi(u.Gid)
		}
	}
	if len(uids) == 0 {
		return nil, 0, errors.New("helper.allowed_users must name at least one user")
	}
	return uids, gid, nil
}

func executable(name string) (string, error) {
	for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		p := filepath.Join(dir, name)
		if s, e := os.Stat(p); e == nil && s.Mode().IsRegular() && s.Mode()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("required system utility %s unavailable", name)
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	p, err := executable(name)
	if err != nil {
		return "", err
	}
	c, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, p, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "SYSTEMD_PAGER=cat", "SYSTEMD_COLORS=0"}
	cmd.WaitDelay = time.Second
	var b commandBuffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err = cmd.Run()
	if c.Err() != nil {
		return b.String(), c.Err()
	}
	if b.truncated {
		return b.String(), errors.New("command output exceeded 1 MiB")
	}
	return b.String(), err
}

// checkSocketDir refuses to create the socket in a directory other processes
// could plant sockets in: it must be root-owned and not group/world writable.
func checkSocketDir(socket string) error {
	dir := filepath.Dir(socket)
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("helper socket directory %s: %v", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("helper socket directory %s is not a directory", dir)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("helper socket directory %s must not be group/world writable", dir)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || sys.Uid != 0 {
		return fmt.Errorf("helper socket directory %s must be owned by root", dir)
	}
	return nil
}

// Run serves privileged operations until ctx is done or the listener fails.
func Run(ctx context.Context, cfg ServerConfig, log *slog.Logger) error {
	if !ValidAbsPath(cfg.Socket) {
		return errors.New("helper.socket must be an absolute cleaned path")
	}
	if err := checkSocketDir(cfg.Socket); err != nil {
		return err
	}
	uids, gid, err := ResolveUsers(cfg.AllowedUsers)
	if err != nil {
		return err
	}
	if err := ValidateServicesACL(cfg.Services); err != nil {
		return err
	}
	allowed := map[int]bool{0: true} // root always allowed (e.g. manual testing)
	for _, id := range uids {
		allowed[id] = true
	}
	_ = os.Remove(cfg.Socket) // a stale socket from a crashed run; bind below fails on anything live
	l, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return fmt.Errorf("listen on %s: %v", cfg.Socket, err)
	}
	defer l.Close()
	if err := os.Chown(cfg.Socket, uids[0], gid); err != nil {
		return fmt.Errorf("set socket owner: %w", err)
	}
	if err := os.Chmod(cfg.Socket, 0o660); err != nil {
		return fmt.Errorf("set socket permissions: %w", err)
	}
	srv := &server{cfg: cfg, allowed: allowed, log: log}
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()
	log.Info("helper_listening", "socket", cfg.Socket, "allowed_users", cfg.AllowedUsers,
		"services", len(cfg.Services), "firewall", cfg.AllowFirewall, "kill", cfg.AllowKill, "update", cfg.AllowUpdate, "sites", cfg.AllowSites, "apps", cfg.AllowApps, "databases", cfg.AllowDatabases)
	slots := make(chan struct{}, 16)
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				_ = os.Remove(cfg.Socket)
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			go func() { defer func() { <-slots }(); srv.handle(conn) }()
		default:
			_ = conn.Close()
		}
	}
}

type server struct {
	cfg     ServerConfig
	allowed map[int]bool
	log     *slog.Logger
}

func (s *server) handle(conn net.Conn) {
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	uid, ok := peerUID(uc)
	if !ok || !s.allowed[int(uid)] {
		s.log.Warn("helper_peer_denied", "uid", uid)
		return
	}
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	var req Request
	if err := json.NewDecoder(bufio.NewReader(io.LimitReader(conn, requestLimit))).Decode(&req); err != nil {
		return
	}
	// Certificate issuance waits on the ACME network round-trip; extend the
	// connection budget for it. Package installs wait on mirrors and can run
	// for several minutes. Everything else keeps the 60s bound.
	if req.Op == OpSite && (req.Action == "issue-cert" || req.Action == "cert-status") {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	}
	if req.Op == OpApp {
		_ = conn.SetDeadline(time.Now().Add(appInstallDeadline))
	}
	resp := s.dispatch(uid, &req)
	_ = json.NewEncoder(conn).Encode(resp)
	s.log.Info("helper_op", "op", req.Op, "uid", uid, "unit", req.Unit, "action", req.Action,
		"engine", req.Engine, "pid", req.PID, "site", req.Site, "path", req.Path, "ok", resp.OK)
}

// dispatch validates the operation against the configured ACLs before doing
// anything privileged. Every branch fails closed.
func (s *server) dispatch(uid int, req *Request) Response {
	switch req.Op {
	case OpService:
		if !CheckServiceACL(s.cfg.Services, req.Unit, req.Action) {
			return Response{Error: fmt.Sprintf("unit %q with action %q is not granted in helper.services; add it to the panel config", req.Unit, req.Action)}
		}
		out, err := run(context.Background(), "systemctl", "--no-ask-password", req.Action, "--", req.Unit)
		return respond(out, err)
	case OpFirewallStatus:
		if !s.cfg.AllowFirewall {
			return Response{Error: "firewall status requires allow_firewall = true in [helper]"}
		}
		if !ValidEngine(req.Engine) {
			return Response{Error: "unsupported firewall engine"}
		}
		args, err := FirewallStatusArgs(req.Engine)
		if err != nil {
			return Response{Error: err.Error()}
		}
		out, err := run(context.Background(), firewallBinary(req.Engine), args...)
		return respond(out, err)
	case OpFirewall:
		if !s.cfg.AllowFirewall {
			return Response{Error: "firewall changes require allow_firewall = true in [helper]"}
		}
		if !ValidEngine(req.Engine) {
			return Response{Error: "unsupported firewall engine"}
		}
		args, err := FirewallArgs(req.Engine, req.Port, req.Protocol, req.Action)
		if err != nil {
			return Response{Error: err.Error()}
		}
		out, err := run(context.Background(), firewallBinary(req.Engine), args...)
		return respond(out, err)
	case OpKill:
		if !s.cfg.AllowKill {
			return Response{Error: "process signaling requires allow_kill = true in [helper]"}
		}
		return s.kill(req)
	case OpUpdate:
		if !s.cfg.AllowUpdate {
			return Response{Error: "self-update requires allow_update = true in [helper]"}
		}
		return s.installUpdate(uid, req)
	case OpSite:
		return s.site(req)
	case OpApp:
		if !s.cfg.AllowApps {
			return Response{Error: "app installation requires allow_apps = true in [helper]"}
		}
		if !ValidAppAction(req.Action) {
			return Response{Error: "unsupported app action"}
		}
		return s.appInstall(req)
	case OpDatabase:
		return s.database(req)
	default:
		return Response{Error: "unknown operation"}
	}
}

func respond(out string, err error) Response {
	if err != nil {
		return Response{Output: TrimOutput(out), Error: err.Error()}
	}
	return Response{OK: true, Output: out}
}

// kill re-checks process identity exactly like the panel does before
// signaling, as root: pidfd pinning plus the start time from /proc.
func (s *server) kill(req *Request) Response {
	if req.PID <= 1 || req.PID == os.Getpid() || (req.Signal != 15 && req.Signal != 9) || req.StartTime == "" {
		return Response{Error: "invalid PID/signal/process identity"}
	}
	fd, err := unix.PidfdOpen(req.PID, 0)
	if err != nil {
		return Response{Error: "process unavailable or cannot be signaled"}
	}
	defer unix.Close(fd)
	b, err := os.ReadFile("/proc/" + strconv.Itoa(req.PID) + "/stat")
	if err != nil {
		return Response{Error: "process disappeared"}
	}
	if !sameProcessStartTime(string(b), req.StartTime) {
		return Response{Error: "process changed; refresh first"}
	}
	if err = unix.PidfdSendSignal(fd, unix.Signal(req.Signal), nil, 0); err != nil {
		return Response{Error: "signal failed: " + err.Error()}
	}
	return Response{OK: true}
}

func fileOwner(st os.FileInfo) (int, bool) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid), true
	}
	return 0, false
}

// sameProcessStartTime extracts field 22 (starttime) the same way the panel's
// parseStat does, without importing the panel package.
func sameProcessStartTime(stat, want string) bool {
	right := strings.LastIndex(stat, ")")
	if right < 0 {
		return false
	}
	f := strings.Fields(stat[right+1:])
	if len(f) < 20 {
		return false
	}
	return f[19] == want
}

// installUpdate verifies the staged release asset against SHA256SUMS, installs
// it over the helper's own binary and restarts the panel unit. The staging
// directory must be owned by the connecting panel user and not group/world
// writable, so a local attacker cannot pre-stage a trojaned payload.
func (s *server) installUpdate(uid int, req *Request) Response {
	dir, err := SanitizeDir(req.Dir)
	if err != nil {
		return Response{Error: err.Error()}
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return Response{Error: "staging directory unavailable: " + err.Error()}
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return Response{Error: "staging directory must be a real directory"}
	}
	if st.Mode().Perm()&0o022 != 0 {
		return Response{Error: "staging directory must not be group/world writable"}
	}
	if owner, ok := fileOwner(st); !ok || owner != uid {
		return Response{Error: "staging directory must be owned by the panel user"}
	}
	asset := "lightpanel-linux-" + runtime.GOARCH
	assetPath := filepath.Join(dir, asset)
	manifestPath := filepath.Join(dir, "SHA256SUMS")
	for _, p := range []string{assetPath, manifestPath} {
		fs, err := os.Lstat(p)
		if err != nil {
			return Response{Error: "missing staged file " + filepath.Base(p)}
		}
		if !fs.Mode().IsRegular() || fs.Mode().Perm()&0o022 != 0 {
			return Response{Error: "staged file " + filepath.Base(p) + " must be a regular file without group/world write"}
		}
		if owner, ok := fileOwner(fs); !ok || owner != uid {
			return Response{Error: "staged file " + filepath.Base(p) + " must be owned by the panel user"}
		}
	}
	if err := verifyChecksum(manifestPath, asset, assetPath); err != nil {
		return Response{Error: "checksum verification failed: " + err.Error()}
	}
	exe, err := os.Executable()
	if err != nil {
		return Response{Error: "cannot locate panel binary"}
	}
	if err := installFile(assetPath, exe); err != nil {
		return Response{Error: "install failed: " + err.Error()}
	}
	if unit := s.cfg.PanelUnit; unit != "" {
		// Restart with a short delay so the panel can flush its response
		// before systemd stops it. The helper is a separate unit and stays up.
		time.AfterFunc(1500*time.Millisecond, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, err := run(ctx, "systemctl", "restart", unit); err != nil {
				s.log.Error("helper_update_restart", "unit", unit, "error", err.Error())
			}
		})
	}
	return Response{OK: true}
}

func verifyChecksum(manifest, asset, target string) error {
	data, err := os.ReadFile(manifest)
	if err != nil {
		return err
	}
	var expected string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 2 && fields[1] == asset {
			expected = strings.ToLower(fields[0])
		}
	}
	if len(expected) != 64 {
		return errors.New("manifest has no entry for " + asset)
	}
	f, err := os.Open(target)
	if err != nil {
		return err
	}
	defer f.Close()
	sum := sha256.New()
	if _, err = io.Copy(sum, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != expected {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expected, got)
	}
	return nil
}

// installFile copies src over dst atomically: same-directory temporary file,
// fsync, chmod 0755, rename. A crash mid-install leaves the old binary intact.
func installFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".lightpanel-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(in, updateExeMax+1))
	if err != nil {
		tmp.Close()
		return err
	}
	if n > updateExeMax {
		tmp.Close()
		return errors.New("update binary exceeds 64 MiB")
	}
	if err = tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func peerUID(conn *net.UnixConn) (uid int, ok bool) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Ucred
	var sockErr error
	if err = raw.Control(func(fd uintptr) {
		cred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, false
	}
	if sockErr != nil || cred == nil {
		return 0, false
	}
	return int(cred.Uid), true
}
