package helper

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestCheckServiceACL(t *testing.T) {
	acl := map[string][]string{
		"nginx.service": {"start", "restart", "enable", "disable", "reload"},
		"*":             {"stop"},
	}
	cases := []struct {
		unit, action string
		want         bool
	}{
		{"nginx.service", "start", true},
		{"nginx.service", "restart", true},
		{"nginx.service", "stop", true}, // union: the "*" wildcard also grants stop
		{"caddy.service", "stop", true}, // wildcard unit
		{"caddy.service", "start", false},
		{"evil", "start", false}, // not a unit
		{"nginx.service", "enable", true},
		{"nginx.service", "disable", true},
		{"nginx.service", "reload", true},
		{"caddy.service", "enable", false},
		{"nginx.service", "mask", false}, // not a whitelisted action
		{"", "start", false},
		{"../../etc/passwd.service", "start", false},
	}
	for _, c := range cases {
		if got := CheckServiceACL(acl, c.unit, c.action); got != c.want {
			t.Errorf("CheckServiceACL(%q, %q) = %v, want %v", c.unit, c.action, got, c.want)
		}
	}
	if CheckServiceACL(nil, "nginx.service", "start") {
		t.Error("nil ACL must deny everything")
	}
}

func TestValidateServicesACL(t *testing.T) {
	if err := (ValidateServicesACL)(map[string][]string{"nginx.service": {"start"}}); err != nil {
		t.Errorf("valid ACL rejected: %v", err)
	}
	if err := (ValidateServicesACL)(map[string][]string{"nginx": {"start"}}); err == nil {
		t.Error("non-.service unit accepted")
	}
	if err := (ValidateServicesACL)(map[string][]string{"nginx.service": {"mask"}}); err == nil {
		t.Error("non-whitelisted action accepted")
	}
	if err := (ValidateServicesACL)(map[string][]string{"nginx.service": {}}); err != nil {
		t.Errorf("explicit empty grant should be allowed: %v", err)
	}
	if err := (ValidateServicesACL)(map[string][]string{"*": {"start", "stop", "restart", "enable", "disable", "reload"}}); err != nil {
		t.Errorf("wildcard unit rejected: %v", err)
	}
}

func TestFirewallArgsWhitelist(t *testing.T) {
	cases := []struct {
		engine, port, protocol, action string
		want                           []string
		wantErr                        bool
	}{
		{"ufw", "443", "tcp", "allow", []string{"allow", "443/tcp"}, false},
		{"ufw", "443", "udp", "remove-deny", []string{"--force", "delete", "deny", "443/udp"}, false},
		{"firewalld", "80", "tcp", "allow", []string{"--add-port=80/tcp"}, false},
		{"firewalld", "80", "tcp", "deny", nil, true},
		{"ufw", "0", "tcp", "allow", nil, true},
		{"ufw", "70000", "tcp", "allow", nil, true},
		{"ufw", "1e3", "tcp", "allow", nil, true}, // no exotic numeric syntax
		{"ufw", "443", "sctp", "allow", nil, true},
		{"ufw", "443", "tcp", "; rm -rf /", nil, true},
		{"iptables", "443", "tcp", "allow", nil, true},
	}
	for _, c := range cases {
		got, err := FirewallArgs(c.engine, c.port, c.protocol, c.action)
		if c.wantErr {
			if err == nil {
				t.Errorf("FirewallArgs(%q,%q,%q,%q) accepted, want reject", c.engine, c.port, c.protocol, c.action)
			}
			continue
		}
		if err != nil {
			t.Errorf("FirewallArgs(%q,%q,%q,%q): %v", c.engine, c.port, c.protocol, c.action, err)
			continue
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("FirewallArgs(%q,%q,%q,%q) = %v, want %v", c.engine, c.port, c.protocol, c.action, got, c.want)
		}
	}
	if _, err := FirewallStatusArgs("ufw"); err != nil {
		t.Errorf("ufw status: %v", err)
	}
	if _, err := FirewallStatusArgs("nft"); err == nil {
		t.Error("unknown engine accepted for status")
	}
}

func TestValidUnit(t *testing.T) {
	valid := []string{"nginx.service", "user@1000.service", "lightpanel.service", "a.service"}
	for _, u := range valid {
		if !ValidUnit(u) {
			t.Errorf("ValidUnit(%q) = false", u)
		}
	}
	invalid := []string{"", "nginx", "nginx.service\n", "-x.service", "nginx.target", "nginx.service;rm"}
	for _, u := range invalid {
		if ValidUnit(u) {
			t.Errorf("ValidUnit(%q) = true", u)
		}
	}
}

func TestSanitizeDir(t *testing.T) {
	if _, err := SanitizeDir("/var/lib/lightpanel/update"); err != nil {
		t.Errorf("valid dir rejected: %v", err)
	}
	for _, bad := range []string{"", "var/lib", "/a/../b", "/a/./b", "\\\\srv", "/a\x00b", "/a\\b", "/var/lib/lightpanel/update/", "/"} {
		if _, err := SanitizeDir(bad); err == nil {
			t.Errorf("SanitizeDir(%q) accepted", bad)
		}
	}
}

func TestValidPortString(t *testing.T) {
	for _, s := range []string{"1", "80", "65535"} {
		if !ValidPortString(s) {
			t.Errorf("ValidPortString(%q) = false", s)
		}
	}
	for _, s := range []string{"0", "65536", "+80", "080", " 80", "", "0x50"} {
		if ValidPortString(s) {
			t.Errorf("ValidPortString(%q) = true", s)
		}
	}
}

func TestClientNilFailsClosed(t *testing.T) {
	var c *Client
	if _, err := c.Call(context.Background(), Request{Op: OpService, Unit: "a.service", Action: "start"}); !errors.Is(err, ErrHelper) {
		t.Errorf("nil client: got %v", err)
	}
}

func TestClientRejectsDeniedResponse(t *testing.T) {
	sock := t.TempDir() + "/s.sock"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		var req Request
		_ = json.NewDecoder(conn).Decode(&req)
		_ = json.NewEncoder(conn).Encode(Response{OK: false, Error: "denied by acl"})
		conn.Close()
	}()
	c := &Client{Socket: sock}
	_, err = c.Call(context.Background(), Request{Op: OpService, Unit: "a.service", Action: "start"})
	if err == nil || !strings.Contains(err.Error(), "denied by acl") || !errors.Is(err, ErrHelper) {
		t.Errorf("denied response not surfaced: %v", err)
	}
}

func TestClientAcceptsOKResponse(t *testing.T) {
	sock := t.TempDir() + "/s.sock"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		var req Request
		_ = json.NewDecoder(conn).Decode(&req)
		_ = json.NewEncoder(conn).Encode(Response{OK: true, Output: "done"})
		conn.Close()
	}()
	out, err := (&Client{Socket: sock}).Call(context.Background(), Request{Op: OpService, Unit: "a.service", Action: "start"})
	if err != nil || out != "done" {
		t.Errorf("Call() = %q, %v", out, err)
	}
}

func TestClientHelloReturnsProtocolVersion(t *testing.T) {
	sock := t.TempDir() + "/s.sock"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		var req Request
		_ = json.NewDecoder(conn).Decode(&req)
		_ = json.NewEncoder(conn).Encode(Response{OK: true, Version: ProtocolVersion})
		conn.Close()
	}()
	v, err := (&Client{Socket: sock}).Hello(context.Background())
	if err != nil || v != ProtocolVersion {
		t.Errorf("Hello() = %d, %v", v, err)
	}
}

func TestClientFlagsStaleHelperOnError(t *testing.T) {
	sock := t.TempDir() + "/s.sock"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		var req Request
		_ = json.NewDecoder(conn).Decode(&req)
		// What an upgraded panel hears from a pre-handshake helper: no
		// version field, and a bare rejection for an operation it predates.
		_ = json.NewEncoder(conn).Encode(Response{Error: "unknown operation"})
		conn.Close()
	}()
	_, err = (&Client{Socket: sock}).Call(context.Background(), Request{Op: OpApp, Action: "install", App: "nginx"})
	if err == nil || !strings.Contains(err.Error(), "unknown operation") {
		t.Fatalf("expected the helper error to surface, got %v", err)
	}
	if !strings.Contains(err.Error(), "systemctl restart lightpanel-helper") {
		t.Errorf("stale helper not flagged: %v", err)
	}
}

func TestClientNoHintOnMatchingVersion(t *testing.T) {
	sock := t.TempDir() + "/s.sock"
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	defer l.Close()
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		var req Request
		_ = json.NewDecoder(conn).Decode(&req)
		_ = json.NewEncoder(conn).Encode(Response{Error: "denied by acl", Version: ProtocolVersion})
		conn.Close()
	}()
	_, err = (&Client{Socket: sock}).Call(context.Background(), Request{Op: OpService, Unit: "a.service", Action: "start"})
	if err == nil || strings.Contains(err.Error(), "systemctl restart") {
		t.Errorf("matching version should not carry the restart hint: %v", err)
	}
}
