//go:build linux

package terminal

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"lightpanel/config"
)

func runtimeSocket(t *testing.T, revoked chan struct{}) (*Manager, *websocket.Conn, <-chan struct{}) {
	t.Helper()
	m := New(&config.Config{AdminUser: "test"})
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		defer close(finished)
		m.run(r, ws, revoked)
	}))
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close(); m.Close(); server.Close() })
	return m, ws, finished
}

func TestPTYResizeAndJobCleanup(t *testing.T) {
	_, ws, finished := runtimeSocket(t, make(chan struct{}))
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","rows":32,"cols":100}`)); err != nil {
		t.Fatal(err)
	}
	// Unknown control frames are ignored, not fatal.
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"ping"}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("stty size; sleep 120 & printf 'CHILD:%s\\n' $!; wait\r")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	var pid string
	pattern := regexp.MustCompile(`CHILD:([0-9]+)`)
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for pid == "" || !strings.Contains(output.String(), "32 100") {
		_, b, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("output so far: %q", output.String())
		}
		output.Write(b)
		match := pattern.FindStringSubmatch(output.String())
		if len(match) > 1 {
			pid = match[1]
		}
	}
	ws.Close()
	waitClosed(t, finished)
	// SIGKILL is asynchronous: on a loaded runner the job can still be
	// runnable with the kill pending when the session handler returns, so
	// wait for the zombie (unreaped) or reaped-and-gone state instead of
	// snapshotting /proc once. A job still alive after the deadline is a
	// real leak.
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, err := os.ReadFile("/proc/" + pid + "/stat")
		if err != nil {
			return // reaped by init, nothing left to assert
		}
		end := strings.LastIndexByte(string(b), ')')
		fields := strings.Fields(string(b[end+1:]))
		if len(fields) == 0 || fields[0] == "Z" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal job still running: %s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFinalOutputBeforeShellExit(t *testing.T) {
	for range 8 {
		_, ws, finished := runtimeSocket(t, make(chan struct{}))
		if err := ws.WriteMessage(websocket.BinaryMessage, []byte("printf 'FINAL_%s\\n' RESULT; exit\r")); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		ws.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, b, err := ws.ReadMessage()
			if err != nil {
				break
			}
			output.Write(b)
		}
		waitClosed(t, finished)
		if !strings.Contains(output.String(), "FINAL_RESULT") {
			t.Fatalf("final output lost: %q", output.String())
		}
	}
}

func TestRevocation(t *testing.T) {
	revoked := make(chan struct{})
	_, ws, finished := runtimeSocket(t, revoked)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("printf 'HELLO_%s\\n' TERMINAL\r")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for !strings.Contains(output.String(), "HELLO_TERMINAL") {
		_, b, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		output.Write(b)
	}
	close(revoked)
	if reason := readUntilClosed(t, ws); reason != "session_revoked" {
		t.Fatal(reason)
	}
	waitClosed(t, finished)
}

func TestShutdownClosesSessions(t *testing.T) {
	m, ws, finished := runtimeSocket(t, make(chan struct{}))
	go m.Close()
	if reason := readUntilClosed(t, ws); reason != "server_shutdown" {
		t.Fatal(reason)
	}
	waitClosed(t, finished)
}

func waitClosed(t *testing.T, finished <-chan struct{}) {
	t.Helper()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("PTY cleanup did not finish")
	}
}

func readUntilClosed(t *testing.T, ws *websocket.Conn) string {
	t.Helper()
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			if e, ok := err.(*websocket.CloseError); ok {
				return e.Text
			}
			t.Fatalf("expected close frame: %v", err)
		}
	}
}
