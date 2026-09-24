//go:build linux

package sysinfo

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }
func TestUploadLimitCleansPartial(t *testing.T) {
	dir := t.TempDir()
	f, e := NewFiles(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r := httptest.NewRequest("POST", "/api/file/upload?path=large", io.LimitReader(zeroReader{}, MaxUpload+1))
	w := httptest.NewRecorder()
	f.Upload(w, r)
	if w.Code != 413 {
		t.Fatalf("expected 413 got %d", w.Code)
	}
	if _, e = os.Stat(filepath.Join(dir, "large")); !os.IsNotExist(e) {
		t.Fatalf("partial upload retained: %v", e)
	}
}
func TestFileModesAndSymlinkSwap(t *testing.T) {
	dir := t.TempDir()
	f, e := NewFiles(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	outside := t.TempDir()
	if e = os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(filepath.Join(dir, "nested"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(filepath.Join(dir, "nested"), filepath.Join(dir, "old")); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(outside, filepath.Join(dir, "nested")); e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	f.Upload(w, httptest.NewRequest("POST", "/api/file/upload?path=nested/new", strings.NewReader("x")))
	if w.Code == 200 {
		t.Fatal("upload escaped through changed directory")
	}
	if _, e = os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(e) {
		t.Fatal("outside file created")
	}
	r := httptest.NewRequest("POST", "/api/file/chmod", strings.NewReader("path=x&mode=4777"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	f.Chmod(w, r)
	if w.Code != 400 {
		t.Fatal("setuid mode accepted")
	}
}
func TestMetricsCounters(t *testing.T) {
	total, idle, e := cpuCounters("cpu 100 20 30 400 50 6 7 8 99 88\n")
	if e != nil || total != 621 || idle != 450 {
		t.Fatalf("double counted guest: %d %d %v", total, idle, e)
	}
	rx, tx := netCounters("lo: 500 0 0 0 0 0 0 0 500 0 0 0 0 0 0 0\neth0: 1000 0 0 0 0 0 0 0 2000 0 0 0 0 0 0 0")
	if rx != 1000 || tx != 2000 {
		t.Fatal(rx, tx)
	}
	m := &MetricsReader{}
	v := m.Read()
	if v.MemoryTotal == 0 || len(v.Load) != 3 {
		t.Fatal("no Linux metrics")
	}
	if m.Read().Uptime != v.Uptime {
		t.Fatal("cache not used")
	}
}
func TestManagerFailureAndBoundedOutput(t *testing.T) {
	calls := 0
	m := &Manager{Run: func(context.Context, string, ...string) (string, error) {
		calls++
		return "denied", errors.New("exit 1")
	}}
	w := httptest.NewRecorder()
	m.Logs(w, httptest.NewRequest("GET", "/api/logs?name=--help", nil))
	if w.Code != 400 || calls != 0 {
		t.Fatal("bad unit reached command")
	}
	w = httptest.NewRecorder()
	m.Logs(w, httptest.NewRequest("GET", "/api/logs", nil))
	if w.Code != 502 {
		t.Fatal("command failure hidden")
	}
	var b limitedBuffer
	p := make([]byte, outputLimit+10)
	n, e := b.Write(p)
	if e != nil || n != len(p) || b.Len() != outputLimit || !b.truncated {
		t.Fatal("unbounded command output")
	}
	if _, e := RunCommand(context.Background(), "sh", "-c", "true"); !errors.Is(e, ErrUnavailable) {
		t.Fatal("unapproved executable")
	}
}
func TestSignalOwnedChildWithIdentity(t *testing.T) {
	cmd := exec.Command("/bin/sleep", "60")
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	b, e := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/stat")
	if e != nil {
		t.Fatal(e)
	}
	p, e := parseStat(string(b))
	if e != nil {
		t.Fatal(e)
	}
	send := func(start string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/process/kill", strings.NewReader("pid="+strconv.Itoa(p.PID)+"&signal=15&start_time="+start))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		HandleProcessKill(w, r)
		return w
	}
	if w := send("wrong"); w.Code != 409 {
		t.Fatalf("identity mismatch: %d", w.Code)
	}
	w := send(p.StartTime)
	if w.Code == 501 {
		t.Skip("kernel lacks pidfd")
	}
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not terminate")
	}
}
