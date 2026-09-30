//go:build linux

package helper

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func testServer(allowTerminal bool) *server {
	return &server{
		cfg: ServerConfig{AllowTerminal: allowTerminal},
		log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
}

// dialRelay wires a net.Pipe to a terminalRelay goroutine and returns the
// panel side after the handshake response.
func dialRelay(t *testing.T, s *server) (net.Conn, *Response) {
	t.Helper()
	panel, helperSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.terminalRelay(0, helperSide)
	}()
	t.Cleanup(func() {
		panel.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("terminal relay did not return after hangup")
		}
	})
	_ = panel.SetDeadline(time.Now().Add(10 * time.Second))
	var resp Response
	if err := json.NewDecoder(bufio.NewReader(panel)).Decode(&resp); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	_ = panel.SetDeadline(time.Time{})
	return panel, &resp
}

func TestTerminalRelayDeniesWithoutACL(t *testing.T) {
	_, resp := dialRelay(t, testServer(false))
	if resp.OK {
		t.Fatal("relay allowed without allow_terminal")
	}
	if !strings.Contains(resp.Error, "allow_terminal") {
		t.Fatalf("error %q does not point at allow_terminal", resp.Error)
	}
	if resp.Version != ProtocolVersion {
		t.Fatalf("response version %d", resp.Version)
	}
}

func TestTerminalRelayRootShell(t *testing.T) {
	panel, resp := dialRelay(t, testServer(true))
	if !resp.OK {
		t.Fatalf("relay rejected: %s", resp.Error)
	}
	if err := WriteResizeFrame(panel, 30, 100); err != nil {
		t.Fatal(err)
	}
	if err := WriteInputFrame(panel, []byte("stty size\r")); err != nil {
		t.Fatal(err)
	}
	output := readRelayOutput(t, panel, "30 100", 10*time.Second)
	if !strings.Contains(output, "30 100") {
		t.Fatalf("resize not applied, output %q", output)
	}
}

func TestTerminalRelayEndsOnShellExit(t *testing.T) {
	panel, resp := dialRelay(t, testServer(true))
	if !resp.OK {
		t.Fatalf("relay rejected: %s", resp.Error)
	}
	if err := WriteInputFrame(panel, []byte("exit\r")); err != nil {
		t.Fatal(err)
	}
	// The shell exits, the relay flushes the final output and hangs up: the
	// panel reads EOF. The relay goroutine (tracked by dialRelay's cleanup)
	// must return promptly instead of leaking the session.
	_ = panel.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4096)
	for {
		if _, err := panel.Read(buf); err != nil {
			return
		}
	}
}

// readRelayOutput drains raw relay output until needle appears.
func readRelayOutput(t *testing.T, conn net.Conn, needle string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	_ = conn.SetReadDeadline(deadline)
	var output strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(output.String(), needle) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %q; output so far %q", needle, output.String())
		}
		n, err := conn.Read(buf)
		if n > 0 {
			output.Write(buf[:n])
		}
		if err != nil {
			t.Fatalf("relay closed early waiting for %q; output so far %q", needle, output.String())
		}
	}
	return output.String()
}
