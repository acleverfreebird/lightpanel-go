//go:build linux

package terminal

import (
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lightpanel/pkg/helper"
)

// fakeRelay serves the terminal handshake on a unix socket and echoes framed
// input back as raw output — a minimal stand-in for the helper's relay. With
// deny set it rejects the handshake like a helper without allow_terminal.
func fakeRelay(t *testing.T, socket string, deny bool) {
	t.Helper()
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			serveFakeRelay(t, conn, deny)
		}
	}()
	t.Cleanup(func() {
		l.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
}

func serveFakeRelay(t *testing.T, conn net.Conn, deny bool) {
	defer conn.Close()
	var req helper.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil || req.Op != helper.OpTerminal {
		return
	}
	if deny {
		_ = json.NewEncoder(conn).Encode(helper.Response{OK: false, Error: "terminal relay requires allow_terminal = true in [helper]", Version: helper.ProtocolVersion})
		return
	}
	_ = json.NewEncoder(conn).Encode(helper.Response{OK: true, Version: helper.ProtocolVersion})
	for {
		kind, payload, err := helper.ReadRelayFrame(conn)
		if err != nil {
			return
		}
		if kind == helper.FrameInput {
			if _, err := conn.Write(payload); err != nil {
				return
			}
		}
	}
}

func TestRelaySessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "relay.sock")
	fakeRelay(t, sock, false)
	sess, err := startRelay(sock)
	if err != nil {
		t.Fatal(err)
	}
	if sess.pid() != 0 || sess.fields()[3] != "helper" {
		t.Fatalf("unexpected relay session audit: pid=%d fields=%v", sess.pid(), sess.fields())
	}
	select {
	case <-sess.exited():
		t.Fatal("session ended before the relay hung up")
	default:
	}
	if err := sess.input([]byte("ECHO_INPUT")); err != nil {
		t.Fatal(err)
	}
	if err := sess.resize(30, 100); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	var output strings.Builder
	deadline := time.Now().Add(3 * time.Second)
	_ = sess.conn.SetReadDeadline(deadline)
	for !strings.Contains(output.String(), "ECHO_INPUT") {
		if time.Now().After(deadline) {
			t.Fatalf("timeout; output %q", output.String())
		}
		n, err := sess.output(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil {
			t.Fatalf("output ended early: %v, got %q", err, output.String())
		}
	}
	// The relay hanging up must surface through exited(); drain like
	// pumpOutput does so exit detection stays exercised.
	sess.kill()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for {
			if _, err := sess.output(buf); err != nil {
				return
			}
		}
	}()
	select {
	case <-sess.exited():
	case <-time.After(3 * time.Second):
		t.Fatal("kill() did not end the session")
	}
	<-drained
	sess.release()
}

func TestStartRelayRejectsDenial(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "relay.sock")
	fakeRelay(t, sock, true)
	if _, err := startRelay(sock); err == nil || !strings.Contains(err.Error(), "allow_terminal") {
		t.Fatalf("denial not surfaced: %v", err)
	}
}

func TestStartRelayFailsWithoutHelper(t *testing.T) {
	if _, err := startRelay(filepath.Join(t.TempDir(), "missing.sock")); err == nil {
		t.Fatal("dialing a dead helper must fail")
	}
}
