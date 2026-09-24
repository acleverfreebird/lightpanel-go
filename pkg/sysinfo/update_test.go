//go:build linux

package sysinfo

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		remote, local string
		want          int
	}{
		{"v1.2.3", "v1.2.3", 0},
		{"v1.2.4", "v1.2.3", 1},
		{"v1.3.0", "v1.2.9", 1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.2.3", "v1.2.10", -1},
		{"v1.2.3", "v1.2.3-rc1", 1},
		{"v1.2.3-rc1", "v1.2.3", -1},
		{"v1.2.3-rc1", "v1.2.3-rc1", 0},
		{"1.2.3", "v1.2.3", 0},
		{"v0.9.0", "dev", 1}, // dev falls back to string comparison
	}
	for _, c := range cases {
		got := compareVersion(c.remote, c.local)
		sign := 0
		if got > 0 {
			sign = 1
		} else if got < 0 {
			sign = -1
		}
		if sign != c.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", c.remote, c.local, got, c.want)
		}
	}
}

func fakeReleaseServer(t *testing.T, payload, sums []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"), strings.HasSuffix(r.URL.Path, "/tags/v2.0.0"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(payload))
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			_, _ = w.Write(sums)
		case strings.HasSuffix(r.URL.Path, "/lightpanel-linux-amd64"), strings.HasSuffix(r.URL.Path, "/lightpanel-linux-arm64"):
			_, _ = w.Write([]byte("NEW-BINARY-CONTENT"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func overrideUpdateTargets(t *testing.T, srv *httptest.Server, version string) (string, chan struct{}) {
	t.Helper()
	previousAPI, previousDownload, previousVersion, previousRestart := updateAPIBase, updateDownloadBase, BuildVersion, scheduleRestart
	exe := filepath.Join(t.TempDir(), "lightpanel")
	if err := os.WriteFile(exe, []byte("OLD-BINARY"), 0755); err != nil {
		t.Fatal(err)
	}
	updateAPIBase, updateDownloadBase, BuildVersion = srv.URL+"/api", srv.URL, version
	executablePath = func() (string, error) { return exe, nil }
	restarted := make(chan struct{}, 1)
	scheduleRestart = func() { restarted <- struct{}{} }
	t.Cleanup(func() {
		updateAPIBase, updateDownloadBase, BuildVersion = previousAPI, previousDownload, previousVersion
		executablePath = os.Executable
		scheduleRestart = previousRestart
	})
	return exe, restarted
}

func TestUpdateCheck(t *testing.T) {
	srv := fakeReleaseServer(t, []byte(`{"tag_name":"v2.0.0","html_url":"https://github.com/x/y/releases/tag/v2.0.0"}`), nil)
	overrideUpdateTargets(t, srv, "v1.0.0")
	w := httptest.NewRecorder()
	UpdateCheck(w, httptest.NewRequest("GET", "/check", nil))
	if w.Code != 200 {
		t.Fatalf("check %d %s", w.Code, w.Body)
	}
	var status UpdateStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Current != "v1.0.0" || status.Latest != "v2.0.0" || !status.Update {
		t.Fatalf("status %+v", status)
	}
}

func TestUpdateCheckNoRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	overrideUpdateTargets(t, srv, "v1.0.0")
	w := httptest.NewRecorder()
	UpdateCheck(w, httptest.NewRequest("GET", "/check", nil))
	if w.Code != 200 {
		t.Fatalf("check %d %s", w.Code, w.Body)
	}
	var status UpdateStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Latest != "" || status.Update {
		t.Fatalf("status %+v", status)
	}
}

func TestUpdateApplyReplacesBinary(t *testing.T) {
	payload := `{"tag_name":"v2.0.0","html_url":"https://github.com/x/y/releases/tag/v2.0.0"}`
	asset := fmt.Sprintf("%x", sha256.Sum256([]byte("NEW-BINARY-CONTENT")))
	sums := []byte(asset + "  lightpanel-linux-amd64\n" + asset + "  lightpanel-linux-arm64\n")
	srv := fakeReleaseServer(t, []byte(payload), sums)
	exe, restarted := overrideUpdateTargets(t, srv, "v1.0.0")

	r := httptest.NewRequest("POST", "/apply", strings.NewReader("tag=v2.0.0"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	UpdateApply(w, r)
	if w.Code != 200 {
		t.Fatalf("apply %d %s", w.Code, w.Body)
	}
	b, err := os.ReadFile(exe)
	if err != nil || string(b) != "NEW-BINARY-CONTENT" {
		t.Fatalf("binary not replaced: %q %v", b, err)
	}
	info, err := os.Stat(exe)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("mode %v %v", info, err)
	}
	select {
	case <-restarted:
	default:
		t.Fatal("restart not scheduled")
	}
	// temporary artifacts are cleaned up
	entries, _ := os.ReadDir(filepath.Dir(exe))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".lightpanel-update-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestUpdateApplyRejectsBadChecksum(t *testing.T) {
	payload := `{"tag_name":"v2.0.0","html_url":"https://github.com/x/y/releases/tag/v2.0.0"}`
	sums := []byte(strings.Repeat("0", 64) + "  lightpanel-linux-amd64\n")
	srv := fakeReleaseServer(t, []byte(payload), sums)
	exe, _ := overrideUpdateTargets(t, srv, "v1.0.0")

	r := httptest.NewRequest("POST", "/apply", strings.NewReader("tag=v2.0.0"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	UpdateApply(w, r)
	if w.Code == 200 {
		t.Fatal("bad checksum accepted")
	}
	if b, err := os.ReadFile(exe); err != nil || string(b) != "OLD-BINARY" {
		t.Fatalf("binary touched despite checksum failure: %q %v", b, err)
	}
}

func TestUpdateApplyUpToDate(t *testing.T) {
	payload := `{"tag_name":"v2.0.0","html_url":"https://github.com/x/y"}`
	sums := []byte("00  lightpanel-linux-amd64\n")
	srv := fakeReleaseServer(t, []byte(payload), sums)
	exe, restarted := overrideUpdateTargets(t, srv, "v2.0.0")

	r := httptest.NewRequest("POST", "/apply", strings.NewReader("tag=v2.0.0"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	UpdateApply(w, r)
	if w.Code != 200 {
		t.Fatalf("apply %d %s", w.Code, w.Body)
	}
	if b, err := os.ReadFile(exe); err != nil || string(b) != "OLD-BINARY" {
		t.Fatal("binary replaced although already up to date")
	}
	select {
	case <-restarted:
		t.Fatal("restart scheduled although already up to date")
	default:
	}
}
