//go:build linux

package sysinfo

import (
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

	"golang.org/x/sys/unix"
)

const MaxUpload = 32 << 20

type Files struct{ root *os.Root }

func NewFiles(dir string) (*Files, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	s, err := root.Stat(".")
	slash, e := os.Stat("/")
	if err != nil || e != nil || os.SameFile(s, slash) {
		root.Close()
		return nil, fmt.Errorf("sandbox cannot be filesystem root")
	}
	return &Files{root: root}, nil
}
func (f *Files) Close() error { return f.root.Close() }
func safeName(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if !fs.ValidPath(p) || strings.ContainsAny(p, "\\\x00") {
		return "", fmt.Errorf("path must be relative without .. or empty components")
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
func (f *Files) regular(name string) (*os.File, error) {
	p, err := safeName(name)
	if err != nil {
		return nil, err
	}
	file, err := f.root.OpenFile(p, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	err = unix.Fstat(int(file.Fd()), &st)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 {
		file.Close()
		return nil, fmt.Errorf("only regular files with one hard link are allowed")
	}
	return file, nil
}

type FileEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	Regular bool   `json:"regular"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
}

func (f *Files) List(w http.ResponseWriter, r *http.Request) {
	p, err := safeName(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	dir, err := f.root.OpenFile(p, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
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
		list = append(list, FileEntry{Name: e.Name(), Path: path.Join(p, e.Name()), IsDir: s.IsDir(), Regular: s.Mode().IsRegular(), Size: s.Size(), Mode: fmt.Sprintf("%03o", s.Mode().Perm())})
	}
	JSON(w, map[string]any{"path": p, "items": list, "offset": offset, "more": more})
}
func (f *Files) Download(w http.ResponseWriter, r *http.Request) {
	file, err := f.regular(r.URL.Query().Get("path"))
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
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": s.Name()}))
	http.ServeContent(w, r, s.Name(), s.ModTime(), file)
}
func (f *Files) Upload(w http.ResponseWriter, r *http.Request) {
	p, err := safeName(r.URL.Query().Get("path"))
	if err != nil || p == "." {
		http.Error(w, "invalid destination", 400)
		return
	}
	// Pin parent directory for creation and cleanup; never truncate an existing inode.
	parent, err := f.root.OpenRoot(path.Dir(p))
	if err != nil {
		fileError(w, err)
		return
	}
	defer parent.Close()
	name := path.Base(p)
	file, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		fileError(w, err)
		return
	}
	success := false
	defer func() {
		file.Close()
		if !success {
			_ = parent.Remove(name)
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, MaxUpload)
	if _, err = io.Copy(file, r.Body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "upload exceeds 32 MiB", 413)
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
	p, err := safeName(r.FormValue("path"))
	if err != nil || p == "." {
		http.Error(w, "cannot delete root or invalid path", 400)
		return
	}
	if err = f.root.Remove(p); err != nil {
		fileError(w, err)
		return
	}
	JSON(w, map[string]string{"message": "removed (directories must be empty)"})
}
func (f *Files) Chmod(w http.ResponseWriter, r *http.Request) {
	s := r.FormValue("mode")
	mode, err := strconv.ParseUint(s, 8, 32)
	if err != nil || len(s) != 3 || mode > 0777 {
		http.Error(w, "mode must be 3 octal digits (000..777)", 400)
		return
	}
	file, err := f.regular(r.FormValue("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	defer file.Close()
	if err = file.Chmod(os.FileMode(mode)); err != nil {
		fileError(w, err)
		return
	}
	JSON(w, map[string]string{"message": "permissions updated"})
}
