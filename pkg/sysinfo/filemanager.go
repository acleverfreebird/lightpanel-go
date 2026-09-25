//go:build linux

package sysinfo

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const (
	MaxUpload = 32 << 20
	MaxEdit   = 1 << 20
)

// Files manages the whole server filesystem. Clients pass absolute paths;
// they are validated lexically (absolute, cleaned, no ".." components) and
// then used directly, so symlink semantics match familiar tools like SFTP.
type Files struct {
	uploadLimit int64
}

func NewFiles(uploadLimit int64) (*Files, error) {
	if uploadLimit <= 0 || uploadLimit > 2<<30 {
		uploadLimit = MaxUpload
	}
	return &Files{uploadLimit: uploadLimit}, nil
}
func (f *Files) Close() error       { return nil }
func (f *Files) UploadLimit() int64 { return f.uploadLimit }

// resolvePath validates an absolute client path and returns the cleaned
// absolute form. ".." components are rejected outright rather than resolved,
// so a request can never silently climb elsewhere from the directory it names.
func resolvePath(p string) (string, error) {
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return "", fmt.Errorf("path must be absolute without \\ or .. components")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("path must not contain .. components")
		}
	}
	p = path.Clean(p)
	if p == "/" {
		return "/", nil
	}
	if !fs.ValidPath(strings.TrimPrefix(p, "/")) {
		return "", fmt.Errorf("path must be absolute without .. or empty components")
	}
	return p, nil
}
func fileError(w http.ResponseWriter, err error) {
	code := 400
	if errors.Is(err, fs.ErrNotExist) {
		code = 404
	}
	if errors.Is(err, fs.ErrPermission) {
		code = 403
	}
	if errors.Is(err, fs.ErrExist) {
		code = 409
	}
	http.Error(w, "file operation failed: "+err.Error(), code)
}

// openRegular opens a regular file for reading. Symlinks are followed (a
// regular file behind /bin/sh is still downloadable); O_NONBLOCK keeps a
// FIFO-shaped target from blocking the open, and the fstat below rejects
// anything that is not a regular file, including devices.
func openRegular(abs string) (*os.File, error) {
	file, err := os.OpenFile(abs, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err = unix.Fstat(int(file.Fd()), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		return nil, fmt.Errorf("only regular files can be read or downloaded")
	}
	return file, nil
}

// virtualTopDirs cannot be meaningfully deleted or moved: removing their
// mount point would only churn the kernel or break the running system.
var virtualTopDirs = map[string]bool{"proc": true, "sys": true, "dev": true, "run": true}

func guardVirtualTopDir(abs string) error {
	top := strings.TrimPrefix(abs, "/")
	if i := strings.IndexByte(top, '/'); i >= 0 {
		top = top[:i]
	}
	if virtualTopDirs[top] {
		return fmt.Errorf("%s is a virtual system directory and cannot be deleted or moved", top)
	}
	return nil
}

type FileEntry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	IsDir    bool   `json:"is_dir"`
	Regular  bool   `json:"regular"`
	Symlink  bool   `json:"symlink"`
	Size     int64  `json:"size"`
	Mode     string `json:"mode"`
	Modified int64  `json:"modified"`
}

func (f *Files) List(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	// O_DIRECTORY without O_NOFOLLOW: browsing through a symlinked directory
	// (usrmerge /bin → usr/bin) should work like in any file manager.
	dir, err := os.OpenFile(abs, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		fileError(w, err)
		return
	}
	defer dir.Close()
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 || offset > 1000000 {
			http.Error(w, "invalid offset", 400)
			return
		}
	}
	for left := offset; left > 0; {
		batch, e := dir.ReadDir(min(left, 256))
		left -= len(batch)
		if e != nil {
			if e != io.EOF {
				fileError(w, e)
				return
			}
			break
		}
	}
	entries, err := dir.ReadDir(201)
	if err != nil && err != io.EOF {
		fileError(w, err)
		return
	}
	more := len(entries) > 200
	if more {
		entries = entries[:200]
	}
	list := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		s, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, FileEntry{Name: e.Name(), Path: path.Join(abs, e.Name()), IsDir: s.IsDir(), Regular: s.Mode().IsRegular(), Symlink: s.Mode()&os.ModeSymlink != 0, Size: s.Size(), Mode: fmt.Sprintf("%03o", s.Mode().Perm()), Modified: s.ModTime().Unix()})
		if s.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Stat(path.Join(abs, e.Name())); err == nil {
				list[len(list)-1].IsDir = target.IsDir()
			}
		}
	}
	JSON(w, map[string]any{"path": abs, "items": list, "offset": offset, "more": more})
}
func (f *Files) Download(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	file, err := openRegular(abs)
	if err != nil {
		fileError(w, err)
		return
	}
	defer file.Close()
	s, err := file.Stat()
	if err != nil {
		fileError(w, err)
		return
	}
	name := path.Base(abs)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	http.ServeContent(w, r, name, s.ModTime(), file)
}
func (f *Files) Upload(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil || abs == "/" {
		http.Error(w, "invalid destination", 400)
		return
	}
	// O_EXCL|O_NOFOLLOW: never truncate or write through an existing entry,
	// so a failed transfer cannot destroy an unrelated file either.
	file, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		fileError(w, err)
		return
	}
	success := false
	defer func() {
		file.Close()
		if !success {
			_ = os.Remove(abs)
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, f.uploadLimit)
	if _, err = io.Copy(file, r.Body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("upload exceeds %d MiB", f.uploadLimit>>20), 413)
		} else {
			fileError(w, err)
		}
		return
	}
	if err = file.Close(); err != nil {
		fileError(w, err)
		return
	}
	success = true
	JSON(w, map[string]string{"message": "uploaded"})
}
func (f *Files) Delete(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.FormValue("path"))
	if err != nil || abs == "/" {
		http.Error(w, "cannot delete root or invalid path", 400)
		return
	}
	if err = guardVirtualTopDir(abs); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if r.FormValue("recursive") == "true" {
		if err = os.RemoveAll(abs); err != nil {
			fileError(w, err)
			return
		}
		JSON(w, map[string]string{"message": "removed recursively"})
		return
	}
	if err = os.Remove(abs); err != nil {
		fileError(w, err)
		return
	}
	JSON(w, map[string]string{"message": "removed (directories must be empty)"})
}
func (f *Files) Mkdir(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.FormValue("path"))
	if err != nil || abs == "/" {
		http.Error(w, "invalid directory path", 400)
		return
	}
	if err = os.MkdirAll(abs, 0750); err != nil {
		fileError(w, err)
		return
	}
	JSON(w, map[string]string{"message": "directory created"})
}
func (f *Files) Rename(w http.ResponseWriter, r *http.Request) {
	from, err := resolvePath(r.FormValue("path"))
	if err != nil || from == "/" {
		http.Error(w, "invalid source path", 400)
		return
	}
	if err = guardVirtualTopDir(from); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	to, err := resolvePath(r.FormValue("to"))
	if err != nil || to == "/" {
		http.Error(w, "invalid destination path", 400)
		return
	}
	if err = guardVirtualTopDir(to); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// Atomic no-clobber semantics, including existing symbolic links.
	if err = unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE); err != nil {
		fileError(w, err)
		return
	}
	JSON(w, map[string]string{"message": "renamed"})
}
func (f *Files) Read(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	file, err := openRegular(abs)
	if err != nil {
		fileError(w, err)
		return
	}
	defer file.Close()
	s, err := file.Stat()
	if err != nil {
		fileError(w, err)
		return
	}
	if s.Size() > MaxEdit {
		http.Error(w, fmt.Sprintf("file exceeds %d MiB edit limit; download and edit locally", MaxEdit>>20), 400)
		return
	}
	// Size may change between Stat and read; cap the copy regardless.
	data, err := io.ReadAll(io.LimitReader(file, MaxEdit+1))
	if err != nil {
		fileError(w, err)
		return
	}
	if len(data) > MaxEdit {
		http.Error(w, fmt.Sprintf("file exceeds %d MiB edit limit; download and edit locally", MaxEdit>>20), 400)
		return
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		http.Error(w, "only UTF-8 text files can be edited in the browser", 400)
		return
	}
	JSON(w, map[string]any{"path": abs, "size": s.Size(), "modified": s.ModTime().Unix(), "content": string(data)})
}
func (f *Files) Write(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil || abs == "/" {
		http.Error(w, "invalid destination", 400)
		return
	}
	// A save must never silently replace a symlink entry with a regular file;
	// edit the link target through its own path instead.
	info, err := os.Lstat(abs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fileError(w, err)
		return
	}
	if info != nil && !info.Mode().IsRegular() {
		http.Error(w, "destination must be a regular file, not a symbolic link or special file", 400)
		return
	}
	// Write to a temporary file first and rename it over the destination, so a
	// failed transfer never destroys the original content.
	var tmp string
	var file *os.File
	var buf [8]byte
	for attempt := 0; ; attempt++ {
		if _, err = rand.Read(buf[:]); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
		tmp = abs + ".lp-edit-" + hex.EncodeToString(buf[:])
		file, err = os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || attempt >= 4 {
			fileError(w, err)
			return
		}
	}
	success := false
	defer func() {
		file.Close()
		if !success {
			_ = os.Remove(tmp)
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, MaxEdit)
	if _, err = io.Copy(file, r.Body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("content exceeds %d MiB edit limit", MaxEdit>>20), 413)
		} else {
			fileError(w, err)
		}
		return
	}
	if info != nil {
		// Atomic replacement must retain the service-readable mode and owner.
		var st unix.Stat_t
		if err = unix.Lstat(abs, &st); err == nil {
			err = file.Chown(int(st.Uid), int(st.Gid))
		}
		if err == nil {
			err = file.Chmod(info.Mode().Perm())
		}
		if err != nil {
			fileError(w, err)
			return
		}
	}
	if err = file.Sync(); err != nil {
		fileError(w, err)
		return
	}
	if err = file.Close(); err != nil {
		fileError(w, err)
		return
	}
	// Rename replaces any existing regular file atomically and keeps the new
	// content intact; it refuses to replace a non-empty directory.
	if err = os.Rename(tmp, abs); err != nil {
		fileError(w, err)
		return
	}
	success = true
	JSON(w, map[string]string{"message": "saved"})
}
func (f *Files) Chmod(w http.ResponseWriter, r *http.Request) {
	s := r.FormValue("mode")
	mode, err := strconv.ParseUint(s, 8, 32)
	if err != nil || len(s) != 3 || mode > 0777 {
		http.Error(w, "mode must be 3 octal digits (000..777)", 400)
		return
	}
	abs, err := resolvePath(r.FormValue("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	// O_NOFOLLOW: permissions of a symlink entry itself are not editable;
	// chmod the link target through its own path instead.
	file, err := os.OpenFile(abs, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		fileError(w, err)
		return
	}
	defer file.Close()
	var st unix.Stat_t
	if err = unix.Fstat(int(file.Fd()), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		http.Error(w, "only regular files and directories can be chmodded", 400)
		return
	}
	if err = file.Chmod(os.FileMode(mode)); err != nil {
		fileError(w, err)
		return
	}
	JSON(w, map[string]string{"message": "permissions updated"})
}
