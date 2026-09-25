//go:build linux

package helper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const certbotTimeout = 280 * time.Second

// runTimeout is run() with an explicit deadline for operations that wait on
// external services (ACME issuance); everything else keeps execTimeout.
func runTimeout(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	p, err := executable(name)
	if err != nil {
		return "", err
	}
	c, cancel := context.WithTimeout(ctx, timeout)
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

func (s *server) site(req *Request) Response {
	if !s.cfg.AllowSites {
		return Response{Error: "site management requires allow_sites = true in [helper]"}
	}
	if !ValidSiteAction(req.Action) {
		return Response{Error: "unsupported site action"}
	}
	switch req.Action {
	case "create":
		return s.siteCreate(req)
	case "delete":
		return respond("", s.siteDelete(req.Path))
	case "reload":
		out, err := siteReload(context.Background(), req.Engine)
		return respond(out, err)
	case "issue-cert":
		out, err := runTimeout(context.Background(), certbotTimeout, "certbot",
			"-n", "--agree-tos", "-m", req.Email, "-d", req.Domain, "--"+req.Engine)
		return respond(out, err)
	case "cert-status":
		out, err := run(context.Background(), "certbot", "certificates")
		return respond(out, err)
	}
	return Response{Error: "unsupported site action"}
}

// siteReload resolves the engine's systemd unit and reloads it. The helper is
// root, so no per-unit ACL applies here; the whole branch is gated by
// allow_sites instead.
func siteReload(ctx context.Context, engine string) (string, error) {
	candidates, ok := EngineUnits[engine]
	if !ok {
		return "", fmt.Errorf("engine must be nginx or apache")
	}
	args := append([]string{"list-unit-files"}, candidates...)
	args = append(args, "--no-legend", "--no-pager")
	out, _ := run(ctx, "systemctl", args...)
	unit := candidates[0]
	for _, c := range candidates {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, c+".service") {
				unit = c
			}
		}
	}
	return run(ctx, "systemctl", "--no-ask-password", "reload", "--", unit+".service")
}

// siteCreate validates the whole request, recomputes every path helper-side
// and only then touches the filesystem. The panel never supplies file content.
func (s *server) siteCreate(req *Request) Response {
	name := req.Site
	if !ValidSiteName(name) {
		return Response{Error: "site name must be 1-32 chars: lowercase letters, digits and hyphens, starting alphanumeric"}
	}
	engine, kind := req.Engine, req.Kind
	if !ValidSiteEngine(engine) || (kind != "static" && kind != "proxy") {
		return Response{Error: "engine must be nginx/apache; kind static/proxy"}
	}
	port, err := strconv.Atoi(req.Port)
	if err != nil || !ValidPort(port) {
		return Response{Error: "port must be 1..65535"}
	}
	root := req.Root
	if kind == "static" && root == "" {
		root = "/var/www/" + name
	}
	if kind == "static" && !ValidDocumentRoot(root) {
		return Response{Error: "invalid site root path"}
	}
	if kind == "proxy" && !ValidProxyTarget(req.ProxyTarget) {
		return Response{Error: "invalid proxy target"}
	}
	confPath, enabledPath := NativeConfPath(engine, name)
	if _, err := os.Stat(confPath); err == nil {
		return Response{Error: "a site with this name already exists"}
	}
	if enabledPath != "" {
		if _, err := os.Lstat(enabledPath); err == nil {
			return Response{Error: "a site with this name already exists"}
		}
	}
	if kind == "static" && root == "/var/www/"+name {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return Response{Error: "cannot create site root: " + err.Error()}
		}
		indexPath := filepath.Join(root, "index.html")
		if _, err := os.Stat(indexPath); errors.Is(err, fs.ErrNotExist) {
			if err := os.WriteFile(indexPath, []byte(PlaceholderIndex), 0o644); err != nil {
				return Response{Error: "cannot create placeholder index: " + err.Error()}
			}
		}
	}
	content, err := SiteConf(engine, kind, name, req.Domain, port, root, req.ProxyTarget)
	if err != nil {
		return Response{Error: err.Error()}
	}
	if err := writeConf(confPath, content); err != nil {
		return Response{Error: "cannot write configuration: " + err.Error()}
	}
	if enabledPath != "" {
		if err := os.Symlink(confPath, enabledPath); err != nil {
			_ = os.Remove(confPath)
			return Response{Error: "cannot enable site: " + err.Error()}
		}
	}
	if out, err := siteReload(context.Background(), engine); err != nil {
		return Response{OK: true, Output: TrimOutput(out), Error: "site created, but reloading " + engine + " failed: " + err.Error()}
	}
	return Response{OK: true}
}

// siteDelete removes a managed configuration: the path must sit inside the
// managed directories and the file must carry the managed marker, so configs
// written by an administrator are never touched.
func (s *server) siteDelete(id string) error {
	if !IsManagedConfPath(id) {
		return errors.New("configuration path is outside the managed directories")
	}
	target := id
	if fi, err := os.Lstat(id); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if link, err := os.Readlink(id); err == nil {
			target = link
		}
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if len(data) > 512<<10 || !bytes.Contains(data, []byte(ManagedMarker)) {
		return errors.New("refusing to delete: configuration was not created by lightpanel")
	}
	if err := os.Remove(id); err != nil {
		return err
	}
	if target != id {
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// writeConfFile writes atomically: temp file in the same directory, fsync,
// rename. An existing file is never overwritten.
func writeConf(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".lightpanel-*.conf")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := tmp.WriteString(content); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
