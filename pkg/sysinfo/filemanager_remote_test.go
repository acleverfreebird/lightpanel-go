//go:build linux

package sysinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"lightpanel/pkg/helper"
)

// startRemoteFiles serves a real privileged helper (allow_files) on a private
// unix socket and routes the Files handlers through it — the exact production
// path of an unprivileged panel. The socket lives in a hidden subdirectory of
// the returned root so listings stay clean; mutate adjusts the helper config
// (e.g. the upload limit). Returns the handlers and the temp root.
func startRemoteFiles(t *testing.T, mutate func(*helper.ServerConfig)) (*Files, string) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(t.TempDir(), "helper.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unavailable: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	u, err := user.Current()
	if err != nil {
		t.Skipf("cannot resolve current user: %v", err)
	}
	cfg := helper.ServerConfig{Socket: sock, Listener: l, AllowedUsers: []string{u.Username}, AllowFiles: true, UploadLimit: MaxUpload}
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_ = helper.Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	f, err := NewFiles(MaxUpload)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { FilesViaHelper = nil })
	FilesViaHelper = &helper.Client{Socket: sock}
	return f, dir
}

func TestRemoteFileLifecycle(t *testing.T) {
	f, dir := startRemoteFiles(t, nil)
	// mkdir, including multi-level
	if code := form(f.Mkdir, "/mkdir", "path="+dir+"/site/assets"); code != 200 {
		t.Fatalf("mkdir %d", code)
	}
	// upload into the new directory, then read it back through the relay
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
	// write replaces content atomically and leaves no temporaries
	w = httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/write?path="+dir+"/site/assets/index.html", strings.NewReader("<h1>updated</h1>")))
	if w.Code != 200 {
		t.Fatalf("write %d %s", w.Code, w.Body)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "site", "assets", "index.html")); err != nil || string(b) != "<h1>updated</h1>" {
		t.Fatalf("written content %q %v", b, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "site", "assets"))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".lp-edit-") || strings.Contains(e.Name(), ".lp-upload-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
	// upload refuses to overwrite anything, including a symlink entry
	os.Symlink(filepath.Join(dir, "site", "assets", "index.html"), filepath.Join(dir, "alias"))
	w = httptest.NewRecorder()
	f.Upload(w, httptest.NewRequest("POST", "/upload?path="+dir+"/alias", strings.NewReader("x")))
	if w.Code != 409 {
		t.Fatalf("upload over symlink %d", w.Code)
	}
	// rename file and directory, then verify the old paths are gone
	if code := form(f.Rename, "/rename", "path="+dir+"/site/assets/index.html&to="+dir+"/site/home.html"); code != 200 {
		t.Fatalf("rename %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "site", "home.html")); err != nil {
		t.Fatal("renamed file missing")
	}
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
	// invalid destinations are rejected with the direct-mode statuses
	if code := form(f.Rename, "/rename", "path="+dir+"/alias&to="+dir+"/../escape"); code == 200 {
		t.Fatal("path traversal accepted")
	}
	if code := form(f.Rename, "/rename", "path=/proc&to="+dir+"/moved"); code == 200 {
		t.Fatal("rename of virtual system directory accepted")
	}
	if code := form(f.Delete, "/delete", "path=/proc&recursive=true"); code == 200 {
		t.Fatal("delete of virtual system directory accepted")
	}
	if code := form(f.Mkdir, "/mkdir", "path=/"); code == 200 {
		t.Fatal("mkdir root accepted")
	}
	// write through a symlink must fail, chmod only through real entries
	w = httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/write?path="+dir+"/alias", strings.NewReader("x")))
	if w.Code == 200 {
		t.Fatal("write followed symlink")
	}
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

func TestRemoteListShape(t *testing.T) {
	f, dir := startRemoteFiles(t, nil)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0600)
	os.Symlink("a.txt", filepath.Join(dir, "link"))
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
	if data.Path != dir || len(data.Items) != 2 {
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

func TestRemoteDownloadStreamsExactBytes(t *testing.T) {
	f, dir := startRemoteFiles(t, nil)
	os.WriteFile(filepath.Join(dir, "data.bin"), []byte("0123456789"), 0600)
	w := httptest.NewRecorder()
	f.Download(w, httptest.NewRequest("GET", "/download?path="+dir+"/data.bin", nil))
	if w.Code != 200 || w.Body.String() != "0123456789" {
		t.Fatalf("download %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Length") != "10" {
		t.Fatalf("content-length %q", w.Header().Get("Content-Length"))
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "data.bin") {
		t.Fatalf("disposition %q", w.Header().Get("Content-Disposition"))
	}
	// A missing file carries the 404 through the helper's status hint.
	w = httptest.NewRecorder()
	f.Download(w, httptest.NewRequest("GET", "/download?path="+dir+"/missing", nil))
	if w.Code != 404 {
		t.Fatalf("missing download %d", w.Code)
	}
	// A file without permission bits maps to 403, the error this feature
	// eliminates for root-owned files in production.
	os.WriteFile(filepath.Join(dir, "locked"), []byte("secret"), 0000)
	w = httptest.NewRecorder()
	f.Download(w, httptest.NewRequest("GET", "/download?path="+dir+"/locked", nil))
	if w.Code != 403 {
		t.Fatalf("locked download %d %s", w.Code, w.Body.String())
	}
}

func TestRemoteUploadTooLargeIs413(t *testing.T) {
	// The helper enforces the upload cap independently of the panel's
	// MaxBytesReader; a helper-side limit of 1 KiB rejects a 2 KiB body.
	f, dir := startRemoteFiles(t, func(cfg *helper.ServerConfig) { cfg.UploadLimit = 1024 })
	w := httptest.NewRecorder()
	f.Upload(w, httptest.NewRequest("POST", "/upload?path="+dir+"/big.bin", strings.NewReader(strings.Repeat("a", 2048))))
	if w.Code != 413 {
		t.Fatalf("oversized upload %d", w.Code)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".lp-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestRemoteEditPreservesMode(t *testing.T) {
	f, dir := startRemoteFiles(t, nil)
	target := filepath.Join(dir, "script")
	os.WriteFile(target, []byte("old"), 0751)
	w := httptest.NewRecorder()
	f.Write(w, httptest.NewRequest("POST", "/write?path="+url.QueryEscape(target), strings.NewReader("new")))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm() != 0751 {
		t.Fatalf("mode=%o", st.Mode().Perm())
	}
}

func TestRemoteEditRejectsInvalidUTF8(t *testing.T) {
	f, dir := startRemoteFiles(t, nil)
	target := filepath.Join(dir, "binary")
	os.WriteFile(target, []byte{0xff, 0xfe, 'a'}, 0600)
	w := httptest.NewRecorder()
	f.Read(w, httptest.NewRequest("GET", "/read?path="+url.QueryEscape(target), nil))
	if w.Code != 400 {
		t.Fatalf("status=%d; browser would corrupt binary", w.Code)
	}
}

func TestRemoteRenameNeverOverwrites(t *testing.T) {
	f, dir := startRemoteFiles(t, nil)
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	os.WriteFile(from, []byte("source"), 0600)
	os.WriteFile(to, []byte("destination"), 0600)
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

func TestRemoteListPagination(t *testing.T) {
	f, dir := startRemoteFiles(t, nil)
	for i := 0; i < 205; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%03d.txt", i)), []byte("x"), 0600)
	}
	w := httptest.NewRecorder()
	f.List(w, httptest.NewRequest("GET", "/files?path="+dir+"&offset=200", nil))
	if w.Code != 200 {
		t.Fatalf("list %d", w.Code)
	}
	var data struct {
		Items  []FileEntry `json:"items"`
		More   bool        `json:"more"`
		Offset int         `json:"offset"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Items) != 5 || data.More || data.Offset != 200 {
		t.Fatalf("page 2 payload: %d items, more=%v, offset=%d", len(data.Items), data.More, data.Offset)
	}
}
