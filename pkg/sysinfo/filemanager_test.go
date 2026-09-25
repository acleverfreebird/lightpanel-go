//go:build linux

package sysinfo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFilePathHandling(t *testing.T) {
	dir := t.TempDir()
	f, err := NewFiles(MaxUpload)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain.txt"), []byte("plain"), 0600); err != nil {
		t.Fatal(err)
	}
	// Lexically invalid targets are rejected before any filesystem access;
	// devices and FIFOs never look like downloadable regular files.
	for _, p := range []string{"../etc/passwd", "etc/passwd", "/etc/../etc/passwd", "/with\\backslash", "/dev/null", dir + "/fifo"} {
		w := httptest.NewRecorder()
		f.Download(w, httptest.NewRequest("GET", "/api/file/download?path="+strings.ReplaceAll(p, "\\", "%5C"), nil))
		if w.Code == 200 {
			t.Errorf("read unsafe path %s", p)
		}
	}
	// A symlink to a regular file is a legitimate download target.
	if err := os.Symlink(filepath.Join(dir, "plain.txt"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.Download(w, httptest.NewRequest("GET", "/api/file/download?path="+dir+"/alias", nil))
	if w.Code != 200 || w.Body.String() != "plain" {
		t.Fatalf("symlink download %d %s", w.Code, w.Body.String())
	}
	for _, body := range []string{"path=/", "path=/proc", "path=/sys", "path=/dev", "path=/run"} {
		r := httptest.NewRequest("POST", "/api/file/delete", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		f.Delete(w, r)
		if w.Code == 200 {
			t.Fatalf("deleted %s", body)
		}
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
	f, err := NewFiles(MaxUpload)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// mkdir, including multi-level
	if code := form(f.Mkdir, "/mkdir", "path="+dir+"/site/assets"); code != 200 {
		t.Fatalf("mkdir %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "site", "assets")); err != nil {
		t.Fatal("nested directories not created")
	}
	// upload into the new directory, then read it back
	w := httptest.NewRecorder()
	f.Upload(w, httptest.NewRequest("POST", "/upload?path="+dir+"/site/assets/index.html", strings.NewReader("<h1>ok</h1>")))
	if w.Code != 200 {
		t.Fatalf("upload %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	f.Read(w, httptest.NewRequest("GET", "/read?path="+dir+"/site/assets/index.html", nil))
	if w.Code != 200 {
		t.Fatalf("read %d %s", w.Code, w.Body)
	}
	var doc struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Modified int64  `json:"modified"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Content != "<h1>ok</h1>" || doc.Modified <= 0 || doc.Path != dir+"/site/assets/index.html" {
		t.Fatalf("read payload %q %d %q", doc.Content, doc.Modified, doc.Path)
	}
	// write replaces content atomically
	w = httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/write?path="+dir+"/site/assets/index.html", strings.NewReader("<h1>updated</h1>")))
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
	f.Read(w, httptest.NewRequest("GET", "/read?path="+dir+"/blob.bin", nil))
	if w.Code != 400 {
		t.Fatalf("binary read %d", w.Code)
	}
	// upload refuses to overwrite anything, including a symlink entry
	if err := os.Symlink(filepath.Join(dir, "blob.bin"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	f.Upload(w, httptest.NewRequest("POST", "/upload?path="+dir+"/alias", strings.NewReader("x")))
	if w.Code != 409 {
		t.Fatalf("upload over symlink %d", w.Code)
	}
	// rename file and verify the old path is gone
	if code := form(f.Rename, "/rename", "path="+dir+"/site/assets/index.html&to="+dir+"/site/home.html"); code != 200 {
		t.Fatalf("rename %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "site", "home.html")); err != nil {
		t.Fatal("renamed file missing")
	}
	if _, err := os.Stat(filepath.Join(dir, "site", "assets", "index.html")); !os.IsNotExist(err) {
		t.Fatal("old path still present")
	}
	// rename accepts directories
	if code := form(f.Rename, "/rename", "path="+dir+"/site/assets&to="+dir+"/site/pages"); code != 200 {
		t.Fatalf("rename dir %d", code)
	}
	// recursive delete removes the tree
	if code := form(f.Delete, "/delete", "path="+dir+"/site&recursive=true"); code != 200 {
		t.Fatalf("recursive delete %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "site")); !os.IsNotExist(err) {
		t.Fatal("directory tree survived recursive delete")
	}
	// invalid destinations are rejected
	if code := form(f.Rename, "/rename", "path="+dir+"/blob.bin&to="+dir+"/../escape"); code == 200 {
		t.Fatal("path traversal accepted")
	}
	if code := form(f.Rename, "/rename", "path="+dir+"/blob.bin&to=/"); code == 200 {
		t.Fatal("rename to root accepted")
	}
	if code := form(f.Rename, "/rename", "path=/proc&to="+dir+"/moved"); code == 200 {
		t.Fatal("rename of virtual system directory accepted")
	}
	if code := form(f.Mkdir, "/mkdir", "path=/"); code == 200 {
		t.Fatal("mkdir root accepted")
	}
	// write through a symlink must fail
	w = httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/write?path="+dir+"/alias", strings.NewReader("x")))
	if w.Code == 200 {
		t.Fatal("write followed symlink")
	}
	// chmod works on directories but never through symlinks or setuid bits
	if code := form(f.Chmod, "/chmod", "path="+dir+"&mode=755"); code != 200 {
		t.Fatalf("chmod dir %d", code)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("dir mode %v %v", info, err)
	}
	if code := form(f.Chmod, "/chmod", "path="+dir+"/alias&mode=700"); code == 200 {
		t.Fatal("chmod through symlink accepted")
	}
}

func TestWriteTooLarge(t *testing.T) {
	f, err := NewFiles(MaxUpload)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/write?path="+t.TempDir()+"/big.txt", strings.NewReader(strings.Repeat("a", MaxEdit+1))))
	if w.Code != 413 {
		t.Fatalf("oversized write %d", w.Code)
	}
}

func TestListReportsModified(t *testing.T) {
	dir := t.TempDir()
	f, err := NewFiles(MaxUpload)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.List(w, httptest.NewRequest("GET", "/files?path="+dir, nil))
	if w.Code != 200 {
		t.Fatalf("list %d", w.Code)
	}
	var data struct {
		Path  string      `json:"path"`
		Items []FileEntry `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Path != dir {
		t.Fatalf("list path %q", data.Path)
	}
	if len(data.Items) != 2 {
		t.Fatalf("list payload %+v", data.Items)
	}
	for _, item := range data.Items {
		if item.Path != dir+"/"+item.Name {
			t.Fatalf("entry path %q", item.Path)
		}
		if item.Name == "a.txt" && (!item.Regular || item.Modified <= 0 || item.Size != 2) {
			t.Fatalf("file entry %+v", item)
		}
		if item.Name == "link" && (!item.Symlink || item.Regular || item.IsDir) {
			t.Fatalf("symlink entry %+v", item)
		}
	}
}
