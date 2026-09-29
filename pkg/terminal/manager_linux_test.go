//go:build linux

package terminal

import (
	"net/http/httptest"
	"testing"

	"lightpanel/config"
)

func TestShutdownAdmission(t *testing.T) {
	m := New(&config.Config{})
	if !m.track() {
		t.Fatal("fresh manager must admit sessions")
	}
	m.untrack()
	m.Close()
	if m.track() {
		t.Fatal("closed manager must reject new sessions")
	}
}

func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		origin, host string
		want         bool
	}{
		// Browsers always send Origin on websocket handshakes; its host must
		// match the request's Host. Non-browser clients may omit it.
		{"http://panel", "panel", true},
		{"http://panel:8888", "panel:8888", true},
		{"", "panel", true},
		{"http://evil", "panel", false},
		{"http://panel/", "panel", false},
		{"https://panel", "panel", false},
		{"null", "panel", false},
	} {
		r := httptest.NewRequest("GET", "http://"+tc.host+"/ws/terminal", nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		if got := sameOrigin(r, &config.Config{PublicOrigin: "http://0.0.0.0:8888"}); got != tc.want {
			t.Fatalf("sameOrigin(origin=%q host=%q) = %v, want %v", tc.origin, tc.host, got, tc.want)
		}
	}
}
