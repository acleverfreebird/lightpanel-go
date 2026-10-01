//go:build linux

package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// relayChunkSize mirrors the panel's frame chunking; the helper accepts any
// frame up to ReadRelayFrame's 1 MiB cap.
const relayChunkSize = 256 << 10

// startFileHelper serves a real helper on a private unix socket inside the
// test's temp directory (no root needed thanks to ServerConfig.Listener) and
// returns a client connected to it.
func startFileHelper(t *testing.T, mutate func(*ServerConfig)) *Client {
	t.Helper()
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
	cfg := ServerConfig{Socket: sock, Listener: l, AllowedUsers: []string{u.Username}, AllowFiles: true, UploadLimit: 32 << 20}
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Run(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	return &Client{Socket: sock}
}

// relayBody uploads/saves body through the framed relay and returns the final
// verdict. An expected handshake rejection (e.g. the no-overwrite pre-check)
// comes back as the response of the failed Relay call.
func relayBody(t *testing.T, c *Client, action, target string, body []byte) *Response {
	t.Helper()
	conn, resp, err := c.Relay(context.Background(), Request{Op: OpFile, Action: action, Path: target})
	if err != nil {
		if resp == nil {
			t.Fatalf("relay handshake: %v", err)
		}
		return resp
	}
	defer conn.Close()
	if len(body) > 0 {
		if err = WriteInputFrame(conn, body); err != nil {
			t.Fatalf("send frame: %v", err)
		}
	}
	if err = WriteCommitFrame(conn); err != nil {
		t.Fatalf("send commit: %v", err)
	}
	var final Response
	if err = json.NewDecoder(conn).Decode(&final); err != nil {
		t.Fatalf("read verdict: %v", err)
	}
	return &final
}

func relayStoreBody(t *testing.T, c *Client, target string, body []byte) *Response {
	t.Helper()
	return relayBody(t, c, "store", target, body)
}

func TestFileOpRequiresAllowFiles(t *testing.T) {
	c := startFileHelper(t, func(cfg *ServerConfig) { cfg.AllowFiles = false })
	_, err := c.Call(context.Background(), Request{Op: OpFile, Action: "list", Path: "/"})
	if err == nil || !strings.Contains(err.Error(), "allow_files") {
		t.Fatalf("expected allow_files rejection, got %v", err)
	}
}

func TestFileCallOpsSemantics(t *testing.T) {
	c := startFileHelper(t, nil)
	dir := t.TempDir()
	ctx := context.Background()

	// mkdir is MkdirAll semantics.
	if _, err := c.Call(ctx, Request{Op: OpFile, Action: "mkdir", Path: dir + "/site/assets"}); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, err := os.Stat(dir + "/site/assets"); err != nil {
		t.Fatal("nested directories not created")
	}
	// mkdir of the root is refused.
	if _, err := c.Call(ctx, Request{Op: OpFile, Action: "mkdir", Path: "/"}); err == nil {
		t.Fatal("mkdir root accepted")
	}
	// relative paths are refused on this side too
	if _, err := c.Call(ctx, Request{Op: OpFile, Action: "mkdir", Path: "rel/path"}); err == nil {
		t.Fatal("relative path accepted")
	}

	// rename with no-clobber: an existing target fails with 409.
	os.WriteFile(dir+"/plain.txt", []byte("body"), 0600)
	os.WriteFile(dir+"/taken.txt", []byte("x"), 0600)
	if _, err := c.Call(ctx, Request{Op: OpFile, Action: "rename", Path: dir + "/plain.txt", To: dir + "/taken.txt"}); err == nil || !strings.Contains(err.Error(), "file exists") {
		t.Fatalf("rename onto existing entry: %v", err)
	}
	// virtual top dir guards re-checked here
	if _, err := c.Call(ctx, Request{Op: OpFile, Action: "delete", Path: "/proc", Recursive: true}); err == nil {
		t.Fatal("delete /proc accepted")
	}
	if _, err := c.Call(ctx, Request{Op: OpFile, Action: "rename", Path: "/dev", To: dir + "/moved"}); err == nil {
		t.Fatal("rename /dev accepted")
	}

	// chmod: dir works, symlink and oversized mode refused.
	os.Symlink(dir+"/plain.txt", dir+"/alias")
	if _, err := c.Call(ctx, Request{Op: OpFile, Action: "chmod", Path: dir + "/alias", Mode: 0o700}); err == nil {
		t.Fatal("chmod through symlink accepted")
	}
	if _, err := c.Call(ctx, Request{Op: OpFile, Action: "chmod", Path: dir + "/plain.txt", Mode: 0o1000}); err == nil {
		t.Fatal("oversized mode accepted")
	}
	if _, err := c.Call(ctx, Request{Op: OpFile, Action: "chmod", Path: dir, Mode: 0o755}); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("dir mode %v %v", st, err)
	}
}

func TestFileListAndRead(t *testing.T) {
	c := startFileHelper(t, nil)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hi"), 0600)
	os.Symlink("a.txt", filepath.Join(dir, "link"))

	out, err := c.Call(context.Background(), Request{Op: OpFile, Action: "list", Path: dir})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var data struct {
		Path   string      `json:"path"`
		Items  []FileEntry `json:"items"`
		More   bool        `json:"more"`
		Offset int         `json:"offset"`
	}
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatal(err)
	}
	if data.Path != dir || len(data.Items) != 2 || data.More || data.Offset != 0 {
		t.Fatalf("listing %+v", data)
	}
	for _, item := range data.Items {
		if item.Name == "a.txt" && (!item.Regular || item.Size != 2 || item.Modified <= 0 || item.Mode != "600") {
			t.Fatalf("file entry %+v", item)
		}
		if item.Name == "link" && (!item.Symlink || item.IsDir || item.Regular) {
			t.Fatalf("symlink entry %+v", item)
		}
	}
	// read returns the UTF-8 content; binary content is refused with 400.
	out, err = c.Call(context.Background(), Request{Op: OpFile, Action: "read", Path: dir + "/a.txt"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Content != "hi" {
		t.Fatalf("read payload %q %v", out, err)
	}
	os.WriteFile(filepath.Join(dir, "blob.bin"), []byte{0x00, 0x01}, 0600)
	if _, err := c.Call(context.Background(), Request{Op: OpFile, Action: "read", Path: dir + "/blob.bin"}); err == nil {
		t.Fatal("binary read accepted")
	}
	if _, err := c.Call(context.Background(), Request{Op: OpFile, Action: "read", Path: dir + "/missing.txt"}); err == nil {
		t.Fatal("missing read accepted")
	}
}

func TestFileFetchRelay(t *testing.T) {
	c := startFileHelper(t, nil)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "data.bin"), []byte("download-me"), 0600)

	conn, resp, err := c.Relay(context.Background(), Request{Op: OpFile, Action: "fetch", Path: dir + "/data.bin"})
	if err != nil {
		t.Fatalf("fetch handshake: %v", err)
	}
	defer conn.Close()
	var meta struct {
		Size int64 `json:"size"`
	}
	if resp == nil || json.Unmarshal([]byte(resp.Output), &meta) != nil || meta.Size != 11 {
		t.Fatalf("fetch metadata %+v", resp)
	}
	body, err := io.ReadAll(conn)
	if err != nil || string(body) != "download-me" {
		t.Fatalf("fetch stream %q %v", body, err)
	}
	// Directories and missing files are refused with status hints.
	_, resp, err = c.Relay(context.Background(), Request{Op: OpFile, Action: "fetch", Path: dir})
	if err == nil || resp == nil || resp.Code != 400 {
		t.Fatalf("directory fetch %+v %v", resp, err)
	}
	_, resp, err = c.Relay(context.Background(), Request{Op: OpFile, Action: "fetch", Path: dir + "/missing"})
	if err == nil || resp == nil || resp.Code != 404 {
		t.Fatalf("missing fetch %+v %v", resp, err)
	}
}

func TestFileStoreRelayLifecycle(t *testing.T) {
	c := startFileHelper(t, nil)
	dir := t.TempDir()
	target := filepath.Join(dir, "uploaded.bin")

	final := relayStoreBody(t, c, target, []byte("uploaded-bytes"))
	if !final.OK {
		t.Fatalf("store verdict %+v", final)
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "uploaded-bytes" {
		t.Fatalf("stored content %q %v", data, err)
	}
	// The upload never overwrites: an existing entry fails with 409 before
	// any body is transferred.
	final = relayStoreBody(t, c, target, []byte("again"))
	if final.OK || final.Code != 409 {
		t.Fatalf("overwrite verdict %+v", final)
	}
	if data, _ := os.ReadFile(target); string(data) != "uploaded-bytes" {
		t.Fatal("existing file was overwritten")
	}
	// No temporary residue in the destination directory.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".lp-upload-") {
			t.Fatalf("temporary upload file left behind: %s", e.Name())
		}
	}
}

func TestFileStoreRelayAbortLeavesNothing(t *testing.T) {
	c := startFileHelper(t, nil)
	dir := t.TempDir()
	target := filepath.Join(dir, "aborted.bin")

	conn, _, err := c.Relay(context.Background(), Request{Op: OpFile, Action: "store", Path: target})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err = WriteInputFrame(conn, []byte("half a body")); err != nil {
		t.Fatalf("frame: %v", err)
	}
	conn.Close() // no commit frame: the helper must discard the transfer
	deadline := time.Now().Add(3 * time.Second)
	for {
		_, statErr := os.Stat(target)
		_, tmpErr := os.Stat(dir)
		if statErr != nil && !os.IsNotExist(statErr) {
			t.Fatalf("stat target: %v", statErr)
		}
		entries, _ := os.ReadDir(dir)
		leftover := false
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".lp-upload-") {
				leftover = true
			}
		}
		if !leftover && os.IsNotExist(statErr) && tmpErr == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("aborted transfer left target=%v leftoverTmp=%v", statErr == nil, leftover)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFileStoreRelayCap(t *testing.T) {
	c := startFileHelper(t, func(cfg *ServerConfig) { cfg.UploadLimit = 8 })
	dir := t.TempDir()
	final := relayStoreBody(t, c, filepath.Join(dir, "big.bin"), []byte("0123456789"))
	if final.OK || final.Code != 413 {
		t.Fatalf("cap verdict %+v", final)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("capped transfer left %d files behind", len(entries))
	}
}

func TestFileSaveRelayPreservesMode(t *testing.T) {
	c := startFileHelper(t, nil)
	dir := t.TempDir()
	target := filepath.Join(dir, "script")
	os.WriteFile(target, []byte("old"), 0o751)

	save := func(path string, body []byte) *Response {
		return relayBody(t, c, "save", path, body)
	}
	if final := save(target, []byte("new")); !final.OK {
		t.Fatalf("save verdict %+v", final)
	}
	data, _ := os.ReadFile(target)
	if string(data) != "new" {
		t.Fatalf("saved content %q", data)
	}
	if st, err := os.Stat(target); err != nil || st.Mode().Perm() != 0o751 {
		t.Fatalf("mode not preserved: %v %v", st, err)
	}
	// Saving through a symlink entry is refused (edit the target instead).
	os.Symlink(target, filepath.Join(dir, "alias"))
	if final := save(filepath.Join(dir, "alias"), []byte("x")); final.OK || final.Code != 400 {
		t.Fatalf("symlink save verdict %+v", final)
	}
	// Saving a new file works and leaves no temporary residue.
	if final := save(filepath.Join(dir, "new.txt"), []byte("created")); !final.OK {
		t.Fatalf("new-file save verdict %+v", final)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".lp-edit-") {
			t.Fatalf("temporary edit file left behind: %s", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatalf("new file missing: %v", err)
	}
}

func TestFilePathValidationRejectsTraversal(t *testing.T) {
	c := startFileHelper(t, nil)
	for _, bad := range []string{"", "/etc/../etc/passwd", "/with\\backslash", "/a\x00b", "/tmp/x/"} {
		if _, err := c.Call(context.Background(), Request{Op: OpFile, Action: "read", Path: bad}); err == nil {
			t.Errorf("invalid path %q accepted", bad)
		}
	}
}

// TestFileRelayFramingRoundTrip uploads a body larger than one frame to
// verify chunk boundaries survive the relay end to end.
func TestFileRelayFramingRoundTrip(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "chunked.bin")
	c := startFileHelper(t, nil)
	conn, _, err := c.Relay(context.Background(), Request{Op: OpFile, Action: "store", Path: target})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer conn.Close()
	// Multiple chunks, including the maximum frame size boundary.
	for _, chunk := range [][]byte{bytes.Repeat([]byte{0xA5}, relayChunkSize), []byte("tail")} {
		if err = WriteInputFrame(conn, chunk); err != nil {
			t.Fatalf("frame: %v", err)
		}
	}
	if err = WriteCommitFrame(conn); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var final Response
	if err = json.NewDecoder(conn).Decode(&final); err != nil {
		t.Fatalf("verdict: %v", err)
	}
	if !final.OK {
		t.Fatalf("chunked store rejected: %+v", final)
	}
	st, err := os.Stat(target)
	if err != nil || st.Size() != int64(relayChunkSize+4) {
		t.Fatalf("stored size %v %v", st, err)
	}
	data, _ := os.ReadFile(target)
	if string(data[relayChunkSize:]) != "tail" {
		t.Fatal("chunk boundaries corrupted the body")
	}
}
