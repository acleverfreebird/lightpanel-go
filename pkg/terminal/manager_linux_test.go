//go:build linux

package terminal

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lightpanel/config"
)

func TestTicketsBoundExpiredAndConsumedAtomically(t *testing.T) {
	m := New(&config.Config{})
	defer m.Close()
	put := func(raw, owner string, expiry time.Time) {
		m.tickets[sha256.Sum256([]byte(raw))] = ticket{owner, expiry}
	}
	raw := token()
	put(raw, "alice", time.Now().Add(time.Minute))
	if m.consume(raw, "bob") || m.consume(raw, "alice") {
		t.Fatal("wrong-session redemption must consume and reject ticket")
	}
	put(raw, "alice", time.Now().Add(-time.Second))
	if m.consume(raw, "alice") {
		t.Fatal("expired ticket accepted")
	}
	put(raw, "alice", time.Now().Add(time.Minute))
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m.consume(raw, "alice") {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("accepted %d redemptions", accepted.Load())
	}
}

func TestQuotaAndShutdown(t *testing.T) {
	m := New(&config.Config{})
	if !m.reserve("a") || !m.reserve("a") || m.reserve("a") || !m.reserve("b") || !m.reserve("b") || m.reserve("c") {
		t.Fatal("quota not enforced")
	}
	m.release("a")
	if !m.reserve("c") {
		t.Fatal("quota not released")
	}
	m.release("a")
	m.release("b")
	m.release("b")
	m.release("c")
	m.Close()
	if m.reserve("a") {
		t.Fatal("shutdown admitted new session")
	}
}

func TestStrictOriginGate(t *testing.T) {
	for _, tc := range []struct {
		origin, host, configured string
		disabled, readonly       bool
		want                     int
	}{
		{"http://panel", "panel", "http://panel", false, false, 204},
		{"", "panel", "http://panel", false, false, 403},
		{"null", "panel", "http://panel", false, false, 403},
		{"http://panel/", "panel", "http://panel", false, false, 403},
		{"https://panel", "panel", "http://panel", false, false, 403},
		{"http://panel", "evil", "http://panel", false, false, 403},
		{"http://panel", "panel", "http://0.0.0.0:8888", false, false, 403},
		{"http://panel", "panel", "http://panel", true, false, 404},
		{"http://panel", "panel", "http://panel", false, true, 403},
	} {
		enabled := !tc.disabled
		m := New(&config.Config{PublicOrigin: tc.configured, TerminalEnabled: &enabled, ReadOnly: tc.readonly})
		r := httptest.NewRequest("GET", "http://panel/ws/terminal", nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Referer", "http://panel/")
		r.Header.Set("X-Forwarded-Host", "panel")
		w := httptest.NewRecorder()
		m.Gate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%+v got %d", tc, w.Code)
		}
		m.Close()
	}
}
