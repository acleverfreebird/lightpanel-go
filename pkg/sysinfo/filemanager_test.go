//go:build linux

package sysinfo

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFileBoundary(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := NewFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, p := range []string{"../secret", "/etc/passwd", "escape", "hard", "fifo"} {
		w := httptest.NewRecorder()
		f.Download(w, httptest.NewRequest("GET", "/api/file/download?path="+p, nil))
		if w.Code == 200 {
			t.Errorf("read unsafe path %s", p)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/file/delete", strings.NewReader("path=."))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	f.Delete(w, r)
	if w.Code == 200 {
		t.Fatal("deleted root")
	}
	r = httptest.NewRequest("POST", "/api/file/upload?path=new.txt", bytes.NewBufferString("hello"))
	w = httptest.NewRecorder()
	f.Upload(w, r)
	if w.Code != 200 {
		t.Fatalf("upload %d %s", w.Code, w.Body)
	}
	r = httptest.NewRequest("POST", "/api/file/upload?path=new.txt", bytes.NewBufferString("overwrite"))
	w = httptest.NewRecorder()
	f.Upload(w, r)
	if w.Code != 409 {
		t.Fatal("overwrote existing file")
	}
	w = httptest.NewRecorder()
	f.Download(w, httptest.NewRequest("GET", "/api/file/download?path=new.txt", nil))
	if w.Body.String() != "hello" {
		t.Fatal(w.Body.String())
	}
}
