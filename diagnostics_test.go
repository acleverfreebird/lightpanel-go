//go:build linux

package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lightpanel/config"
)

func TestDiagnosticsMode(t *testing.T) {
	for _, tt := range []struct {
		name string
		cfg  config.Config
		uid  int
		want string
	}{
		{"root", config.Config{}, 0, "root"},
		{"root ignores helper", config.Config{Helper: &config.HelperConfig{}}, 0, "root"},
		{"helper", config.Config{Helper: &config.HelperConfig{}}, 1000, "helper"},
		{"unprivileged", config.Config{}, 1000, "unprivileged"},
		{"read-only takes precedence", config.Config{ReadOnly: true}, 0, "read-only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := diagnosticsMode(&tt.cfg, tt.uid); got != tt.want {
				t.Fatalf("mode = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDiagnosticsHelperConnectivity(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "helper.sock")
	cfg := &config.Config{Helper: &config.HelperConfig{Socket: socket}}
	if collectDiagnostics(context.Background(), cfg).Helper.Reachable {
		t.Fatal("nonexistent socket reported reachable")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	result := collectDiagnostics(context.Background(), cfg)
	if !result.Helper.Configured || !result.Helper.Reachable {
		t.Fatalf("live socket not detected: %+v", result.Helper)
	}
	listener.(*net.UnixListener).SetDeadline(time.Now().Add(time.Second))
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	var command [1]byte
	if n, err := conn.Read(command[:]); n != 0 || err != io.EOF {
		t.Fatalf("probe sent a command or left connection open: bytes=%d, err=%v", n, err)
	}
}

func TestDiagnosticsToolAvailability(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "systemctl")
	if diagnosticToolAvailable("systemctl", []string{dir}) {
		t.Fatal("missing tool reported available")
	}
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if diagnosticToolAvailable("systemctl", []string{dir}) {
		t.Fatal("nonexecutable file reported available")
	}
	if err := os.Chmod(tool, 0700); err != nil {
		t.Fatal(err)
	}
	if !diagnosticToolAvailable("systemctl", []string{filepath.Join(dir, "missing"), dir}) {
		t.Fatal("executable in later search directory not detected")
	}
	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tool, 0700); err != nil {
		t.Fatal(err)
	}
	if diagnosticToolAvailable("systemctl", []string{dir}) {
		t.Fatal("directory reported as an executable tool")
	}
}

func TestDiagnosticsAuthenticatedAndRedacted(t *testing.T) {
	h, cfg := testPanel(t, true)
	cfg.Helper = &config.HelperConfig{Socket: "/private/secret-helper.sock", StagingDir: "/private/staging", AllowedUsers: []string{"secret-user"}, Services: map[string][]string{"nginx.service": {"restart"}}, AllowFirewall: true}
	if w := request(h, "GET", "/api/health", "", nil, ""); w.Code != 401 {
		t.Fatalf("unauthenticated health = %d", w.Code)
	}
	cookie, _ := login(t, h)
	w := request(h, "GET", "/api/health", "", cookie, "")
	if w.Code != 200 {
		t.Fatalf("health = %d: %s", w.Code, w.Body)
	}
	var got diagnostics
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.UID != os.Geteuid() || got.Mode != "read-only" || !got.ReadOnly {
		t.Fatalf("wrong process identity: %+v", got)
	}
	if !got.Helper.AllowFirewall || got.Helper.ServiceRules != 1 {
		t.Fatalf("missing configured authorization: %+v", got.Helper)
	}
	for _, secret := range []string{cfg.PasswordHash, "/private/", "secret-user"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("health leaks sensitive configuration %q", secret)
		}
	}
}
