//go:build linux

package sysinfo

import (
	"encoding/json"
	"golang.org/x/sys/unix"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRenameNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	os.WriteFile(from, []byte("source"), 0600)
	os.WriteFile(to, []byte("destination"), 0600)
	f, _ := NewFiles(MaxUpload)
	code := form(f.Rename, "/api/file/rename", url.Values{"path": {from}, "to": {to}}.Encode())
	if code != 409 {
		t.Fatalf("status=%d want 409", code)
	}
	data, _ := os.ReadFile(to)
	if string(data) != "destination" {
		t.Fatal("destination overwritten")
	}
	data, _ = os.ReadFile(from)
	if string(data) != "source" {
		t.Fatal("source lost")
	}
}

func TestEditPreservesMode(t *testing.T) {
	target := filepath.Join(t.TempDir(), "script")
	os.WriteFile(target, []byte("old"), 0751)
	f, _ := NewFiles(MaxUpload)
	w := httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/api/file/write?path="+url.QueryEscape(target), strings.NewReader("new")))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm() != 0751 {
		t.Fatalf("mode=%o", st.Mode().Perm())
	}
}

func TestEditRejectsInvalidUTF8(t *testing.T) {
	target := filepath.Join(t.TempDir(), "binary")
	os.WriteFile(target, []byte{0xff, 0xfe, 'a'}, 0600)
	f, _ := NewFiles(MaxUpload)
	w := httptest.NewRecorder()
	f.Read(w, httptest.NewRequest("GET", "/api/file/read?path="+url.QueryEscape(target), nil))
	if w.Code != 400 {
		t.Fatalf("status=%d; browser would corrupt binary", w.Code)
	}
}

func TestEditRejectsSpecialFile(t *testing.T) {
	f, _ := NewFiles(MaxUpload)
	w := httptest.NewRecorder()
	// A harmless directory verifies non-regular target validation before transfer.
	target := t.TempDir()
	f.Write(w, httptest.NewRequest("POST", "/api/file/write?path="+url.QueryEscape(target), strings.NewReader("data")))
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestChmodFIFOIsNonblocking(t *testing.T) {
	target := filepath.Join(t.TempDir(), "pipe")
	if err := unix.Mkfifo(target, 0600); err != nil {
		t.Fatal(err)
	}
	f, _ := NewFiles(MaxUpload)
	done := make(chan int, 1)
	go func() {
		done <- form(f.Chmod, "/api/file/chmod", url.Values{"path": {target}, "mode": {"600"}}.Encode())
	}()
	select {
	case code := <-done:
		if code != 400 {
			t.Fatal(code)
		}
	case <-time.After(time.Second):
		t.Fatal("chmod blocked on FIFO")
	}
}

func TestListDirectorySymlinkIsNavigable(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	f, _ := NewFiles(MaxUpload)
	w := httptest.NewRecorder()
	f.List(w, httptest.NewRequest("GET", "/api/files?path="+url.QueryEscape(dir), nil))
	var result struct {
		Items []FileEntry `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 1 || !result.Items[0].Symlink || !result.Items[0].IsDir {
		t.Fatalf("%+v", result)
	}
}
