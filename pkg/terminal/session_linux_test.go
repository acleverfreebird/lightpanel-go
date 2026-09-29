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

func runtimeSocket(t *testing.T, idle, lifetime time.Duration) (*Manager, *websocket.Conn, chan struct{}, <-chan struct{}) {
	t.Helper()
	m := New(&config.Config{AdminUser: "test"})
	m.idle = idle
	m.lifetime = lifetime
	revoked := make(chan struct{})
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		defer close(finished)
		if !m.reserve("test") {
			return
		}
		defer m.release("test")
		m.run(r, ws, time.Now().Add(time.Hour), revoked)
	}))
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close(); m.Close(); server.Close() })
	return m, ws, revoked, finished
}

func TestOutputLimitAndOutputDoesNotExtendIdle(t *testing.T) {
	for _, tc := range []struct {
		name, command, reason string
		idle                  time.Duration
	}{
		{"output_limit", "yes OUTPUT\r", "output_limit", time.Minute},
		{"output_idle", "while :; do printf tick; sleep 0.01; done\r", "idle_timeout", 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ws, _, finished := runtimeSocket(t, tc.idle, time.Minute)
			if err := ws.WriteMessage(websocket.BinaryMessage, []byte(tc.command)); err != nil {
				t.Fatal(err)
			}
			if reason := readUntilClosed(t, ws); reason != tc.reason {
				t.Fatalf("want %s got %s", tc.reason, reason)
			}
			waitClosed(t, finished)
		})
	}
}

func TestPTYJobCleanupAndResize(t *testing.T) {
	_, ws, _, finished := runtimeSocket(t, time.Minute, time.Minute)
	if err := ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"resize","rows":32,"cols":100}`)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("sleep 120 & printf 'CHILD:%s\\n' $!; wait\r")); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	var pid string
	pattern := regexp.MustCompile(`CHILD:([0-9]+)`)
	ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	for pid == "" {
		_, b, err := ws.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		output.Write(b)
		match := pattern.FindStringSubmatch(output.String())
		if len(match) > 1 {
			pid = match[1]
		}
	}
	ws.Close()
	waitClosed(t, finished)
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err == nil {
		end := strings.LastIndexByte(string(b), ')')
		fields := strings.Fields(string(b[end+1:]))
		if len(fields) == 0 || fields[0] != "Z" {
			t.Fatalf("terminal job still running: %s", b)
		}
	}
}

func TestInputLimit(t *testing.T) {
	_, ws, _, finished := runtimeSocket(t, time.Minute, time.Minute)
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("stty -echo; cat >/dev/null\r")); err != nil {
		t.Fatal(err)
	}
	// Drain output concurrently so only the input cap can terminate the session.
	closed := make(chan string, 1)
	go func() {
		for {
			_, _, err := ws.ReadMessage()
			if err != nil {
				if e, ok := err.(*websocket.CloseError); ok {
					closed <- e.Text
				} else {
					closed <- err.Error()
				}
				return
			}
		}
	}()
	payload := []byte(strings.Repeat("x", 1023) + "\n")
	for range maxInput/len(payload) + 1 {
		if err := ws.WriteMessage(websocket.BinaryMessage, payload); err != nil {
			break
		}
	}
	select {
	case reason := <-closed:
		if reason != "input_limit" {
			t.Fatal(reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("input limit not enforced")
	}
	waitClosed(t, finished)
}

func TestFinalOutputBeforeShellExit(t *testing.T) {
	for range 8 {
		_, ws, _, finished := runtimeSocket(t, time.Minute, time.Minute)
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
func TestRealPTYAndRevocation(t *testing.T) {
	_, ws, revoked, finished := runtimeSocket(t, time.Minute, time.Minute)
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
func TestTimeoutsAndBadInput(t *testing.T) {
	for _, tc := range []struct {
		name       string
		idle, life time.Duration
		payload    []byte
		kind       int
		reason     string
	}{
		{"idle", 50 * time.Millisecond, time.Minute, nil, 0, "idle_timeout"},
		{"absolute", time.Minute, 50 * time.Millisecond, nil, 0, "session_timeout"},
		{"resize", time.Minute, time.Minute, []byte(`{"type":"resize","rows":101,"cols":80}`), websocket.TextMessage, "invalid_resize"},
		{"oversize", time.Minute, time.Minute, make([]byte, 4097), websocket.BinaryMessage, "client_disconnected_or_invalid_frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ws, _, finished := runtimeSocket(t, tc.idle, tc.life)
			if tc.payload != nil {
				if err := ws.WriteMessage(tc.kind, tc.payload); err != nil {
					t.Fatal(err)
				}
			}
			reason := readUntilClosed(t, ws)
			// Gorilla sends its own 1009 close frame for oversized messages.
			if tc.name != "oversize" && reason != tc.reason {
				t.Fatal(reason)
			}
			waitClosed(t, finished)
		})
	}
}
func TestShutdownAndDisconnect(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{true: "shutdown", false: "disconnect"}[shutdown], func(t *testing.T) {
			m, ws, _, finished := runtimeSocket(t, time.Minute, time.Minute)
			if shutdown {
				go m.Close()
				if reason := readUntilClosed(t, ws); reason != "server_shutdown" {
					t.Fatal(reason)
				}
			} else {
				ws.Close()
			}
			waitClosed(t, finished)
		})
	}
}
