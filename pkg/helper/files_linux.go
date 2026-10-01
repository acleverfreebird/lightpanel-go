//go:build linux

package helper

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// Helper-side execution of the file manager. The panel forwards its whole
// whole-filesystem file interface here so an unprivileged panel process gets
// root file semantics (BT-Panel style) instead of 403s on root-owned files.
// As with sites/apps/databases the request contributes only validated paths
// and numbers; every syscall argument is re-derived here, paths are
// re-validated with ValidAbsPath, and the /proc /sys /dev /run guards apply
// again on this side. list/read/mkdir/rename/delete/chmod are ordinary
// request/response calls; fetch/store/save switch the connection to a content
// relay after the JSON handshake (raw bytes helper→panel for fetch, framed
// bytes panel→helper with a trailing commit marker for store/save so an
// interrupted panel can never leave a truncated file behind).

const (
	// fileRelayDeadline bounds one content relay (download stream or upload
	// body). The panel's own HTTP server bounds the browser side to 60s per
	// request, so this is a backstop, not the effective limit.
	fileRelayDeadline = 10 * time.Minute
)

// validFilePath re-validates a client path on the helper side. "/" is only
// meaningful for list (and harmlessly rejected elsewhere), so callers that
// need it pass allowRoot.
func validFilePath(p string, allowRoot bool) (string, error) {
	if allowRoot && p == "/" {
		return "/", nil
	}
	if p == "/" || !ValidAbsPath(p) {
		return "", errors.New("path must be an absolute cleaned path without .. or empty components")
	}
	return p, nil
}

// fileVirtualTopDirs mirrors the panel-side guard: removing or moving these
// mount points only churns the kernel or breaks the running system.
var fileVirtualTopDirs = map[string]bool{"proc": true, "sys": true, "dev": true, "run": true}

func guardFileVirtualTopDir(abs string) error {
	top := strings.TrimPrefix(abs, "/")
	if i := strings.IndexByte(top, '/'); i >= 0 {
		top = top[:i]
	}
	if fileVirtualTopDirs[top] {
		return fmt.Errorf("%s is a virtual system directory and cannot be deleted or moved", top)
	}
	return nil
}

// fileTooLargeError marks a relay body over its size cap; fileResp maps it
// onto 413 like the panel's own MaxBytesError handling.
type fileTooLargeError struct{ msg string }

func (e fileTooLargeError) Error() string { return e.msg }

// fileResp maps a filesystem error onto the HTTP-style status the panel's own
// fileError would have produced, so browser behavior is identical in both
// execution modes.
func fileResp(err error) Response {
	code := 400
	var tooLarge fileTooLargeError
	if errors.As(err, &tooLarge) {
		code = 413
	}
	if errors.Is(err, fs.ErrNotExist) {
		code = 404
	}
	if errors.Is(err, fs.ErrPermission) {
		code = 403
	}
	if errors.Is(err, fs.ErrExist) {
		code = 409
	}
	return Response{OK: false, Error: err.Error(), Code: code}
}

func fileInvalid(msg string) Response {
	return Response{OK: false, Error: msg, Code: 400}
}

// openFileRegular mirrors the panel's openRegular: symlinks are followed,
// O_NONBLOCK keeps a FIFO-shaped target from blocking the open, and the fstat
// rejects anything that is not a regular file.
func openFileRegular(abs string) (*os.File, os.FileInfo, error) {
	file, err := os.OpenFile(abs, os.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	var st unix.Stat_t
	if err = unix.Fstat(int(file.Fd()), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		file.Close()
		return nil, nil, errors.New("only regular files can be read or downloaded")
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, info, nil
}

// fileOp executes the non-relayed file actions inside the request/response
// exchange. The relayed ones (fetch/store/save) are answered by fileRelay.
func (s *server) fileOp(req *Request) Response {
	if !s.cfg.AllowFiles {
		return Response{Error: "file management requires allow_files = true in [helper]", Code: 403}
	}
	if !ValidFileAction(req.Action) {
		return Response{Error: "unsupported file action"}
	}
	switch req.Action {
	case "list":
		return s.fileList(req)
	case "read":
		return s.fileRead(req)
	case "mkdir":
		return s.fileMkdir(req)
	case "rename":
		return s.fileRename(req)
	case "delete":
		return s.fileDelete(req)
	case "chmod":
		return s.fileChmod(req)
	case "copy":
		return s.fileCopy(req)
	case "move":
		return s.fileMove(req)
	case "trash":
		return s.fileTrash(req)
	case "trash-list":
		return s.fileTrashList()
	case "trash-restore":
		return s.fileTrashRestore(req)
	case "trash-delete":
		return s.fileTrashDelete(req)
	case "trash-empty":
		return s.fileTrashEmpty()
	}
	return Response{Error: "unsupported file action"}
}

func (s *server) fileList(req *Request) Response {
	abs, err := validFilePath(req.Path, true)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if req.Offset < 0 || req.Offset > 1000000 {
		return fileInvalid("invalid offset")
	}
	// O_DIRECTORY without O_NOFOLLOW: browsing through a symlinked directory
	// (usrmerge /bin → usr/bin) works like in any file manager.
	dir, err := os.OpenFile(abs, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		return fileResp(err)
	}
	defer dir.Close()
	offset := req.Offset
	for left := offset; left > 0; {
		batch, e := dir.ReadDir(min(left, 256))
		left -= len(batch)
		if e != nil {
			if e != io.EOF {
				return fileResp(e)
			}
			break
		}
	}
	entries, err := dir.ReadDir(201)
	if err != nil && err != io.EOF {
		return fileResp(err)
	}
	more := len(entries) > 200
	if more {
		entries = entries[:200]
	}
	list := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, FileEntry{Name: e.Name(), Path: path.Join(abs, e.Name()), IsDir: info.IsDir(), Regular: info.Mode().IsRegular(), Symlink: info.Mode()&os.ModeSymlink != 0, Size: info.Size(), Mode: fmt.Sprintf("%03o", info.Mode().Perm()), Modified: info.ModTime().Unix()})
		if info.Mode()&os.ModeSymlink != 0 {
			if target, err := os.Stat(path.Join(abs, e.Name())); err == nil {
				list[len(list)-1].IsDir = target.IsDir()
			}
		}
	}
	body, err := json.Marshal(map[string]any{"path": abs, "items": list, "offset": offset, "more": more})
	if err != nil {
		return Response{Error: err.Error(), Code: 500}
	}
	return Response{OK: true, Output: string(body)}
}

func (s *server) fileRead(req *Request) Response {
	abs, err := validFilePath(req.Path, false)
	if err != nil {
		return fileInvalid(err.Error())
	}
	file, info, err := openFileRegular(abs)
	if err != nil {
		return fileResp(err)
	}
	defer file.Close()
	if info.Size() > MaxEditBytes {
		return fileInvalid(fmt.Sprintf("file exceeds %d MiB edit limit; download and edit locally", MaxEditBytes>>20))
	}
	// Size may change between Stat and read; cap the copy regardless.
	data, err := io.ReadAll(io.LimitReader(file, MaxEditBytes+1))
	if err != nil {
		return fileResp(err)
	}
	if len(data) > MaxEditBytes {
		return fileInvalid(fmt.Sprintf("file exceeds %d MiB edit limit; download and edit locally", MaxEditBytes>>20))
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return fileInvalid("only UTF-8 text files can be edited in the browser")
	}
	body, err := json.Marshal(map[string]any{"path": abs, "size": info.Size(), "modified": info.ModTime().Unix(), "content": string(data)})
	if err != nil {
		return Response{Error: err.Error(), Code: 500}
	}
	return Response{OK: true, Output: string(body)}
}

func (s *server) fileMkdir(req *Request) Response {
	abs, err := validFilePath(req.Path, false)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if err := os.MkdirAll(abs, 0750); err != nil {
		return fileResp(err)
	}
	return Response{OK: true}
}

func (s *server) fileRename(req *Request) Response {
	from, err := validFilePath(req.Path, false)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if err = guardFileVirtualTopDir(from); err != nil {
		return fileInvalid(err.Error())
	}
	to, err := validFilePath(req.To, false)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if err = guardFileVirtualTopDir(to); err != nil {
		return fileInvalid(err.Error())
	}
	// Atomic no-clobber semantics, including existing symbolic links.
	if err = unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE); err != nil {
		return fileResp(err)
	}
	return Response{OK: true}
}

func (s *server) fileDelete(req *Request) Response {
	abs, err := validFilePath(req.Path, false)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if err = guardFileVirtualTopDir(abs); err != nil {
		return fileInvalid(err.Error())
	}
	if req.Recursive {
		if err = os.RemoveAll(abs); err != nil {
			return fileResp(err)
		}
		return Response{OK: true}
	}
	if err = os.Remove(abs); err != nil {
		return fileResp(err)
	}
	return Response{OK: true}
}

func (s *server) fileChmod(req *Request) Response {
	if req.Mode > 0o777 {
		return fileInvalid("mode must be 3 octal digits (000..777)")
	}
	abs, err := validFilePath(req.Path, false)
	if err != nil {
		return fileInvalid(err.Error())
	}
	// O_NOFOLLOW: permissions of a symlink entry itself are not editable.
	file, err := os.OpenFile(abs, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return fileResp(err)
	}
	defer file.Close()
	var st unix.Stat_t
	if err = unix.Fstat(int(file.Fd()), &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG && st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fileInvalid("only regular files and directories can be chmodded")
	}
	if err = file.Chmod(os.FileMode(req.Mode)); err != nil {
		return fileResp(err)
	}
	return Response{OK: true}
}

// fileDestDir validates the destination directory of a copy or move: an
// absolute cleaned path that exists and is a real (followed) directory
// outside the virtual system roots.
func fileDestDir(reqTo string) (string, error) {
	to, err := validFilePath(reqTo, false)
	if err != nil {
		return "", err
	}
	if err = guardFileVirtualTopDir(to); err != nil {
		return "", err
	}
	info, err := os.Stat(to)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("destination must be an existing directory")
	}
	return to, nil
}

func (s *server) fileCopy(req *Request) Response {
	from, err := validFilePath(req.Path, false)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if err = guardFileVirtualTopDir(from); err != nil {
		return fileInvalid(err.Error())
	}
	to, err := fileDestDir(req.To)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if err = CopyEntry(from, to); err != nil {
		return fileResp(err)
	}
	return Response{OK: true}
}

func (s *server) fileMove(req *Request) Response {
	from, err := validFilePath(req.Path, false)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if err = guardFileVirtualTopDir(from); err != nil {
		return fileInvalid(err.Error())
	}
	to, err := fileDestDir(req.To)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if err = MoveEntry(from, to); err != nil {
		return fileResp(err)
	}
	return Response{OK: true}
}

func (s *server) fileTrash(req *Request) Response {
	abs, err := validFilePath(req.Path, false)
	if err != nil {
		return fileInvalid(err.Error())
	}
	if err = TrashMovePath(abs); err != nil {
		return fileResp(err)
	}
	return Response{OK: true}
}

func (s *server) fileTrashList() Response {
	items, err := TrashList()
	if err != nil {
		return fileResp(err)
	}
	body, err := json.Marshal(map[string]any{"items": items})
	if err != nil {
		return Response{Error: err.Error(), Code: 500}
	}
	return Response{OK: true, Output: string(body)}
}

// fileTrashAction runs one recycle-bin entry action whose request Path is the
// entry id (never a filesystem path — the id is validated against the id
// alphabet and resolved inside the trash root by the shared implementation).
func (s *server) fileTrashAction(req *Request, op func(string) error) Response {
	if !validTrashID(req.Path) {
		return fileInvalid("invalid recycle-bin entry id")
	}
	if err := op(req.Path); err != nil {
		return fileResp(err)
	}
	return Response{OK: true}
}

func (s *server) fileTrashRestore(req *Request) Response {
	return s.fileTrashAction(req, TrashRestore)
}

func (s *server) fileTrashDelete(req *Request) Response {
	return s.fileTrashAction(req, TrashDelete)
}

func (s *server) fileTrashEmpty() Response {
	cleared, err := TrashEmpty()
	if err != nil {
		return fileResp(err)
	}
	body, err := json.Marshal(map[string]int{"cleared": cleared})
	if err != nil {
		return Response{Error: err.Error(), Code: 500}
	}
	return Response{OK: true, Output: string(body)}
}

// fileRelay serves the content-relaying file actions: it validates the
// request, answers the handshake and then either streams the file to the
// panel (fetch) or receives the body and finalizes it (store/save). One
// connection is handled by this one goroutine end to end; the connection is
// always closed here.
func (s *server) fileRelay(uid int, conn net.Conn, req *Request) {
	log := s.log.With("module", "file_relay", "uid", uid)
	if !s.cfg.AllowFiles {
		writeRelayResponse(conn, Response{OK: false, Error: "file management requires allow_files = true in [helper]", Code: 403})
		_ = conn.Close()
		return
	}
	switch req.Action {
	case "fetch":
		s.relayFetch(conn, req, log)
	case "store":
		s.relayStore(conn, req, log)
	case "save":
		s.relaySave(conn, req, log)
	default:
		writeRelayResponse(conn, fileInvalid("unsupported file action"))
		_ = conn.Close()
	}
}

// relayFetch answers the handshake with the file's size, then streams the raw
// bytes until EOF and closes. The exact Content-Length lets the browser
// surface a truncated transfer instead of silently saving a partial file.
func (s *server) relayFetch(conn net.Conn, req *Request, log *slog.Logger) {
	abs, err := validFilePath(req.Path, false)
	var file *os.File
	var info os.FileInfo
	if err == nil {
		file, info, err = openFileRegular(abs)
	}
	if err != nil {
		writeRelayResponse(conn, fileResp(err))
		_ = conn.Close()
		log.Info("file_fetch_denied", "path", req.Path, "error", err.Error())
		return
	}
	defer file.Close()
	meta, err := json.Marshal(map[string]int64{"size": info.Size(), "modified": info.ModTime().Unix()})
	if err != nil {
		writeRelayResponse(conn, Response{Error: err.Error(), Code: 500})
		_ = conn.Close()
		return
	}
	if !writeRelayResponse(conn, Response{OK: true, Output: string(meta)}) {
		_ = conn.Close()
		return
	}
	_ = conn.SetDeadline(time.Now().Add(fileRelayDeadline))
	_, copyErr := io.Copy(conn, file)
	logErr := ""
	if copyErr != nil {
		logErr = copyErr.Error()
	}
	log.Info("file_fetch", "path", abs, "size", info.Size(), "ok", copyErr == nil, "error", logErr)
	_ = conn.Close()
}

// relayStore receives an upload body as framed chunks into a temporary file
// in the destination directory and, on the commit frame, renames it into
// place with no-clobber semantics. An existing entry (including a symlink)
// rejects the upload before any bytes are transferred, and a connection that
// ends without the commit frame aborts: nothing is installed.
func (s *server) relayStore(conn net.Conn, req *Request, log *slog.Logger) {
	abs, err := validFilePath(req.Path, false)
	if err == nil {
		if _, lerr := os.Lstat(abs); lerr == nil {
			err = fmt.Errorf("%w: %s already exists", fs.ErrExist, abs)
		} else if !errors.Is(lerr, fs.ErrNotExist) {
			err = lerr
		}
	}
	if err == nil && !writeRelayResponse(conn, Response{OK: true}) {
		_ = conn.Close()
		return
	}
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(fileRelayDeadline))
		err = receiveRelayBody(conn, abs, s.uploadLimit(), "upload exceeds %d MiB", nil)
	}
	s.finishFileRelay(conn, req, "file_store", err, log)
}

// relaySave receives the editor's new content like relayStore but replaces an
// existing regular file atomically, preserving its owner and permissions.
func (s *server) relaySave(conn net.Conn, req *Request, log *slog.Logger) {
	abs, err := validFilePath(req.Path, false)
	var preserve os.FileInfo
	if err == nil {
		// A save must never silently replace a symlink entry with a regular
		// file; edit the link target through its own path instead.
		var lerr error
		preserve, lerr = os.Lstat(abs)
		switch {
		case lerr == nil && !preserve.Mode().IsRegular():
			err = errors.New("destination must be a regular file, not a symbolic link or special file")
		case errors.Is(lerr, fs.ErrNotExist):
			// A new file is fine; nothing to preserve.
		default:
			err = lerr
		}
	}
	if err == nil && !writeRelayResponse(conn, Response{OK: true}) {
		_ = conn.Close()
		return
	}
	if err == nil {
		_ = conn.SetDeadline(time.Now().Add(fileRelayDeadline))
		err = receiveRelayBody(conn, abs, MaxEditBytes, "content exceeds %d MiB edit limit", preserve)
	}
	s.finishFileRelay(conn, req, "file_save", err, log)
}

// finishFileRelay answers with the final verdict and closes the connection.
func (s *server) finishFileRelay(conn net.Conn, req *Request, event string, err error, log *slog.Logger) {
	final := Response{OK: true}
	if err != nil {
		final = fileResp(err)
	}
	log.Info(event, "path", req.Path, "ok", final.OK, "code", final.Code)
	final.Version = ProtocolVersion
	_ = json.NewEncoder(conn).Encode(final)
	_ = conn.Close()
}

// receiveRelayBody reads framed chunks until FrameCommit (an EOF or unexpected
// frame aborts) into a same-directory temporary file, then finalizes:
//
//   - upload semantics (preserve == nil): fsync + no-clobber rename over
//     target, so an entry that appeared meanwhile is never overwritten.
//   - save semantics (preserve != nil): chown/chmod from the preserved owner
//     info, fsync, then a plain atomic rename over the existing regular file.
//
// Once the body exceeds limit bytes the transfer aborts with a 413 carrying
// the tooLarge message.
func receiveRelayBody(conn net.Conn, target string, limit int64, tooLarge string, preserve os.FileInfo) error {
	prefix := ".lp-upload-"
	if preserve != nil {
		prefix = ".lp-edit-"
	}
	dir := path.Dir(target)
	var buf [8]byte
	var tmp string
	var file *os.File
	for attempt := 0; ; attempt++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return err
		}
		tmp = path.Join(dir, prefix+hex.EncodeToString(buf[:]))
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			file = f
			break
		}
		if !errors.Is(err, fs.ErrExist) || attempt >= 4 {
			return err
		}
	}
	aborted := false
	defer func() {
		_ = file.Close() // already closed on the success path; harmless
		if aborted {
			_ = os.Remove(tmp)
		}
	}()
	written := int64(0)
	for {
		kind, payload, err := ReadRelayFrame(conn)
		if err != nil {
			aborted = true // EOF or a broken panel: leave nothing behind
			return err
		}
		if kind != FrameInput {
			if kind != FrameCommit {
				aborted = true
				return errors.New("unexpected frame during file transfer")
			}
			break
		}
		written += int64(len(payload))
		if written > limit {
			aborted = true
			return fileTooLargeError{fmt.Sprintf(tooLarge, limit>>20)}
		}
		if _, err = file.Write(payload); err != nil {
			aborted = true
			return err
		}
	}
	if preserve != nil {
		// Atomic replacement must retain the service-readable mode and owner.
		if st, ok := preserve.Sys().(*unix.Stat_t); ok {
			if err := file.Chown(int(st.Uid), int(st.Gid)); err != nil {
				aborted = true
				return err
			}
		}
		if err := file.Chmod(preserve.Mode().Perm()); err != nil {
			aborted = true
			return err
		}
	}
	if err := file.Sync(); err != nil {
		aborted = true
		return err
	}
	if err := file.Close(); err != nil {
		aborted = true
		return err
	}
	if preserve != nil {
		// Rename replaces any existing regular file atomically and keeps the
		// new content intact; it refuses to replace a non-empty directory.
		return os.Rename(tmp, target)
	}
	// No-clobber install: an entry that appeared between the pre-check and
	// the commit still refuses to be overwritten.
	if err := unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, target, unix.RENAME_NOREPLACE); err != nil {
		aborted = true
		return err
	}
	return nil
}

// uploadLimit is the helper-side upload cap, mirroring the panel's
// max_upload_mb so both ends enforce the same bound.
func (s *server) uploadLimit() int64 {
	if s.cfg.UploadLimit <= 0 {
		return 32 << 20
	}
	return s.cfg.UploadLimit
}
