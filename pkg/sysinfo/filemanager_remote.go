//go:build linux

package sysinfo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"time"

	"lightpanel/pkg/helper"
)

// Helper-routed execution of the file manager. When the panel runs
// unprivileged (FilesViaHelper set by main), every operation executes as
// root inside the helper and its result is relayed back, so root-owned files
// stop producing 403s. Browser-visible behavior — paths, sizes, status codes
// and messages — matches the in-process implementation:
//
//   - list/read fetch the JSON body from the helper and pipe it verbatim.
//   - download streams the file bytes over the relay; the helper's size
//     becomes an exact Content-Length so a truncated transfer fails visibly.
//   - upload/save stream the request body to the helper in frames and end
//     with a commit marker; a body error (browser disconnect, size cap)
//     drops the connection, which makes the helper discard its temporary
//     file, so a failed transfer never leaves a partial file behind.
//
// Paths are resolved once here and re-validated by the helper; statuses come
// back in Response.Code and mirror the local fileError mapping.

// fileChunkSize is the framed body chunk used by upload/save relays.
const fileChunkSize = 256 << 10

func fileGatewayStatus(resp *helper.Response) int {
	if resp != nil && resp.Code >= 400 && resp.Code < 600 {
		return resp.Code
	}
	return http.StatusBadGateway
}

func fileGatewayError(w http.ResponseWriter, err error, resp *helper.Response) {
	http.Error(w, "file operation failed: "+err.Error(), fileGatewayStatus(resp))
}

// relayWriteError marks a failure writing to the helper mid-transfer: the
// helper hung up (typically right after aborting an over-limit transfer)
// rather than the request body failing.
type relayWriteError struct{ err error }

func (e *relayWriteError) Error() string { return e.err.Error() }
func (e *relayWriteError) Unwrap() error { return e.err }

// relaySendBody streams body to the helper as framed chunks followed by the
// commit marker. On error the caller drops the connection, which aborts the
// helper-side transfer.
func relaySendBody(conn net.Conn, body io.Reader) error {
	buf := make([]byte, fileChunkSize)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if werr := helper.WriteInputFrame(conn, buf[:n]); werr != nil {
				return &relayWriteError{werr}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if err := helper.WriteCommitFrame(conn); err != nil {
		return &relayWriteError{err}
	}
	return nil
}

// relayVerdict reads the helper's final JSON verdict after a body transfer.
func relayVerdict(conn net.Conn) (*helper.Response, error) {
	var resp helper.Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("helper closed the connection before reporting the result")
		}
		return nil, err
	}
	return &resp, nil
}

// relayTooLarge maps a request-body size failure onto the same 413 message
// the direct implementation produces.
func relayTooLarge(err error, tooLargeFormat string) (int, string, bool) {
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		return 0, "", false
	}
	return 413, fmt.Sprintf(tooLargeFormat, tooLarge.Limit>>20), true
}

func (f *Files) listRemote(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 || offset > 1000000 {
			http.Error(w, "invalid offset", 400)
			return
		}
	}
	resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: "list", Path: abs, Offset: offset})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(resp.Output))
}

func (f *Files) readRemote(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: "read", Path: abs})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(resp.Output))
}

func (f *Files) downloadRemote(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil {
		fileError(w, err)
		return
	}
	conn, resp, err := FilesViaHelper.Relay(r.Context(), helper.Request{Op: helper.OpFile, Action: "fetch", Path: abs})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	defer conn.Close()
	var meta struct {
		Size     int64 `json:"size"`
		Modified int64 `json:"modified"`
	}
	if resp == nil || json.Unmarshal([]byte(resp.Output), &meta) != nil {
		http.Error(w, "file operation failed: helper sent no file metadata", 502)
		return
	}
	name := path.Base(abs)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	// An exact length makes a truncated stream a visible browser error
	// instead of a silently partial download.
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	if _, err = io.Copy(w, conn); err != nil {
		slog.Warn("file_download_relay", "path", abs, "error", err.Error())
	}
}

func (f *Files) uploadRemote(w http.ResponseWriter, r *http.Request) {
	f.relayBodyRemote(w, r, "store", "uploaded", "upload exceeds %d MiB")
}

func (f *Files) writeRemote(w http.ResponseWriter, r *http.Request) {
	f.relayBodyRemote(w, r, "save", "saved", "content exceeds %d MiB edit limit")
}

// relayBodyRemote implements both upload (store: never overwrites) and editor
// save (save: atomic replacement preserving owner and mode) — the request
// body streams to the helper either way.
func (f *Files) relayBodyRemote(w http.ResponseWriter, r *http.Request, action, okMessage, tooLargeFormat string) {
	abs, err := resolvePath(r.URL.Query().Get("path"))
	if err != nil || abs == "/" {
		http.Error(w, "invalid destination", 400)
		return
	}
	conn, resp, err := FilesViaHelper.Relay(r.Context(), helper.Request{Op: helper.OpFile, Action: action, Path: abs})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	defer conn.Close()
	if err = relaySendBody(conn, r.Body); err != nil {
		if status, msg, tooLarge := relayTooLarge(err, tooLargeFormat); tooLarge {
			http.Error(w, msg, status)
			return
		}
		var hangup *relayWriteError
		if errors.As(err, &hangup) {
			// The helper aborts an over-limit transfer by sending its
			// verdict and closing the relay, so later panel writes hit the
			// hangup. The verdict is already buffered on this end; read it
			// (bounded, in case a future helper hangs up silently) instead
			// of collapsing the abort into a 502.
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			final, verr := relayVerdict(conn)
			_ = conn.SetReadDeadline(time.Time{})
			if verr == nil {
				fileGatewayError(w, errors.New(final.Error), final)
				return
			}
		}
		http.Error(w, "file operation failed: "+err.Error(), 502)
		return
	}
	final, err := relayVerdict(conn)
	if err != nil {
		http.Error(w, "file operation failed: "+err.Error(), 502)
		return
	}
	if !final.OK {
		fileGatewayError(w, errors.New(final.Error), final)
		return
	}
	JSON(w, map[string]string{"message": okMessage})
}

func (f *Files) mkdirRemote(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.FormValue("path"))
	if err != nil || abs == "/" {
		http.Error(w, "invalid directory path", 400)
		return
	}
	resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: "mkdir", Path: abs})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	JSON(w, map[string]string{"message": "directory created"})
}

func (f *Files) renameRemote(w http.ResponseWriter, r *http.Request) {
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
	resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: "rename", Path: from, To: to})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	JSON(w, map[string]string{"message": "renamed"})
}

func (f *Files) deleteRemote(w http.ResponseWriter, r *http.Request) {
	abs, err := resolvePath(r.FormValue("path"))
	if err != nil || abs == "/" {
		http.Error(w, "cannot delete root or invalid path", 400)
		return
	}
	if err = guardVirtualTopDir(abs); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: "delete", Path: abs, Recursive: r.FormValue("recursive") == "true"})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	JSON(w, map[string]string{"message": "removed"})
}

func (f *Files) chmodRemote(w http.ResponseWriter, r *http.Request) {
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
	resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: "chmod", Path: abs, Mode: uint32(mode)})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	JSON(w, map[string]string{"message": "permissions updated"})
}

// fileOpError prefers the helper's own verdict message over the wrapped
// transport error, so per-entry batch failures read like the direct mode's.
func fileOpError(err error, resp *helper.Response) error {
	if err != nil && resp != nil && resp.Error != "" {
		return errors.New(resp.Error)
	}
	return err
}

// transferRemote forwards a batch copy or move to the helper one entry at a
// time, aggregating per-entry failures into the same JSON shape as the local
// implementation.
func (f *Files) transferRemote(w http.ResponseWriter, r *http.Request, action string) {
	paths, err := batchPaths(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	to, err := resolvePath(r.FormValue("to"))
	if err != nil || to == "/" {
		http.Error(w, "invalid destination", 400)
		return
	}
	if err = guardVirtualTopDir(to); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// Existence and directory checks belong to the helper (root): the
	// unprivileged panel process often cannot stat the destination itself.
	done, failed := runBatch(paths, validateSource, func(abs string) error {
		resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: action, Path: abs, To: to})
		return fileOpError(err, resp)
	})
	JSON(w, map[string]any{"done": done, "failed": failed})
}

func (f *Files) trashRemote(w http.ResponseWriter, r *http.Request) {
	paths, err := batchPaths(r)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	done, failed := runBatch(paths, validateSource, func(abs string) error {
		resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: "trash", Path: abs})
		return fileOpError(err, resp)
	})
	JSON(w, map[string]any{"done": done, "failed": failed})
}

func (f *Files) trashListRemote(w http.ResponseWriter, r *http.Request) {
	resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: "trash-list"})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(resp.Output))
}

// trashEntryRemote forwards a per-entry trash action whose Path is the entry
// id; the helper validates the id and resolves it inside the trash root.
func (f *Files) trashEntryRemote(w http.ResponseWriter, r *http.Request, action, okMessage string) {
	id := r.FormValue("id")
	if id == "" {
		http.Error(w, "invalid recycle-bin entry id", 400)
		return
	}
	resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: action, Path: id})
	if err != nil {
		fileGatewayError(w, fileOpError(err, resp), resp)
		return
	}
	JSON(w, map[string]string{"message": okMessage})
}

func (f *Files) trashEmptyRemote(w http.ResponseWriter, r *http.Request) {
	resp, err := FilesViaHelper.CallResponse(r.Context(), helper.Request{Op: helper.OpFile, Action: "trash-empty"})
	if err != nil {
		fileGatewayError(w, err, resp)
		return
	}
	JSON(w, map[string]string{"message": "cleared"})
}
