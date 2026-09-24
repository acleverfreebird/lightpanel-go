//go:build linux

package sysinfo

import (
	"bytes"
	"encoding/json"
	"net/http"
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
	f, err := NewFiles(dir, MaxUpload)
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

func form(h http.HandlerFunc, target, body string) int {
	r := httptest.NewRequest("POST", "http://panel"+target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h(w, r)
	return w.Code
}

func TestFileLifecycle(t *testing.T) {
	dir := t.TempDir()
	f, err := NewFiles(dir, MaxUpload)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// mkdir, including multi-level
	if code := form(f.Mkdir, "/mkdir", "path=site/assets"); code != 200 {
		t.Fatalf("mkdir %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "site", "assets")); err != nil {
		t.Fatal("nested directories not created")
	}
	// upload into the new directory, then read it back
	w := httptest.NewRecorder()
	f.Upload(w, httptest.NewRequest("POST", "/upload?path=site/assets/index.html", strings.NewReader("<h1>ok</h1>")))
	if w.Code != 200 {
		t.Fatalf("upload %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	f.Read(w, httptest.NewRequest("GET", "/read?path=site/assets/index.html", nil))
	if w.Code != 200 {
		t.Fatalf("read %d %s", w.Code, w.Body)
	}
	var doc struct {
		Content  string `json:"content"`
		Modified int64  `json:"modified"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Content != "<h1>ok</h1>" || doc.Modified <= 0 {
		t.Fatalf("read payload %q %d", doc.Content, doc.Modified)
	}
	// write replaces content atomically
	w = httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/write?path=site/assets/index.html", strings.NewReader("<h1>updated</h1>")))
	if w.Code != 200 {
		t.Fatalf("write %d %s", w.Code, w.Body)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "site", "assets", "index.html")); err != nil || string(b) != "<h1>updated</h1>" {
		t.Fatalf("written content %q %v", b, err)
	}
	// no leftover temporary files
	entries, _ := os.ReadDir(filepath.Join(dir, "site", "assets"))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".lp-edit-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	// binary content is rejected by the editor read
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01}, 0600); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	f.Read(w, httptest.NewRequest("GET", "/read?path=blob.bin", nil))
	if w.Code != 400 {
		t.Fatalf("binary read %d", w.Code)
	}
	// rename file and verify the old path is gone
	if code := form(f.Rename, "/rename", "path=site/assets/index.html&to=site/home.html"); code != 200 {
		t.Fatalf("rename %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "site", "home.html")); err != nil {
		t.Fatal("renamed file missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "site", "assets", "index.html")); !os.IsNotExist(err) {
		t.Fatal("old path still present")
	}
	// rename accepts directories
	if code := form(f.Rename, "/rename", "path=site/assets&to=site/pages"); code != 200 {
		t.Fatalf("rename dir %d", code)
	}
	// recursive delete removes the tree
	if code := form(f.Delete, "/delete", "path=site&recursive=true"); code != 200 {
		t.Fatalf("recursive delete %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "site")); !os.IsNotExist(err) {
		t.Fatal("directory tree survived recursive delete")
	}
	// invalid rename destinations are rejected
	if code := form(f.Rename, "/rename", "path=blob.bin&to=../escape"); code == 200 {
		t.Fatal("path traversal accepted")
	}
	if code := form(f.Rename, "/rename", "path=blob.bin&to=."); code == 200 {
		t.Fatal("rename to root accepted")
	}
	if code := form(f.Mkdir, "/mkdir", "path=."); code == 200 {
		t.Fatal("mkdir root accepted")
	}
	// write through a symlink must fail
	if err := os.Symlink(filepath.Join(dir, "blob.bin"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/write?path=alias", strings.NewReader("x")))
	if w.Code == 200 {
		t.Fatal("write followed symlink")
	}
}

func TestWriteTooLarge(t *testing.T) {
	f, err := NewFiles(t.TempDir(), MaxUpload)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/write?path=big.txt", strings.NewReader(strings.Repeat("a", MaxEdit+1))))
	if w.Code != 413 {
		t.Fatalf("oversized write %d", w.Code)
	}
}

func TestListReportsModified(t *testing.T) {
	dir := t.TempDir()
	f, err := NewFiles(dir, MaxUpload)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.List(w, httptest.NewRequest("GET", "/files?path=.", nil))
	if w.Code != 200 {
		t.Fatalf("list %d", w.Code)
	}
	var data struct {
		Items []FileEntry `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Items) != 1 || data.Items[0].Name != "a.txt" || data.Items[0].Modified <= 0 || data.Items[0].Size != 2 {
		t.Fatalf("list payload %+v", data.Items)
	}
}
