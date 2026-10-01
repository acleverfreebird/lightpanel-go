//go:build linux

package helper

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Shared execution of the file manager's advanced operations: copy, move and
// the recycle bin. The panel's direct mode (a root panel executing in-process)
// and the privileged helper run exactly this code, so both execution modes
// behave identically; each caller is responsible for validating the paths it
// passes in (resolvePath on the panel, validFilePath plus the virtual
// directory guards on the helper).
//
// Every write is no-clobber: a paste or restore refuses to replace an
// existing entry, matching the panel's upload and rename semantics. Moves on
// one filesystem are a plain rename; across filesystems they copy first and
// only then drop the source, so an interrupted transfer never loses data.

// TrashDir is the recycle bin root. Overridden in tests; on a server it is
// created on demand with 0700 permissions alongside the panel's other state
// under /var/lib/lightpanel.
var TrashDir = "/var/lib/lightpanel/trash"

// fixedTrashNames are the names reserved inside one trash entry directory:
// "item" holds the moved entry, "meta.json" its description.
const (
	trashItemName = "item"
	trashMetaName = "meta.json"
)

// TrashItem is one recycle-bin row as returned by TrashList; it is also the
// JSON shape the browser sees.
type TrashItem struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Original string `json:"original"`
	IsDir    bool   `json:"is_dir"`
	Regular  bool   `json:"regular"`
	Symlink  bool   `json:"symlink"`
	Size     int64  `json:"size"`
	Deleted  int64  `json:"deleted"`
}

// trashMeta is the on-disk description of one trash entry.
type trashMeta struct {
	Original string `json:"original"`
	Name     string `json:"name"`
	IsDir    bool   `json:"is_dir"`
	Deleted  int64  `json:"deleted"`
}

func isCrossDevice(err error) bool {
	return errors.Is(err, unix.EXDEV)
}

// dirContainsDir reports whether child is parent itself or lies inside it,
// used to refuse operations that would nest a directory into its own subtree.
func dirContainsDir(parent, child string) bool {
	return parent == child || strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}

// existsErr shapes a no-clobber refusal like the relay's store path does, so
// the panel maps it onto 409.
func existsErr(what string) error {
	return fmt.Errorf("%w: %s already exists", fs.ErrExist, what)
}

// copyEntry copies one entry (regular file, directory or symlink) from src to
// dst, which must not exist. Directories are walked recursively; ownership is
// preserved when running as root and treated as best-effort otherwise.
func copyEntry(src, dst string, info os.FileInfo) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.Mode().IsRegular():
		// O_NOFOLLOW: never read or write through an entry swapped in behind
		// us; O_NONBLOCK keeps a FIFO that lost its regular shape from hanging.
		srcFile, err := os.OpenFile(src, os.O_RDONLY|unix.O_NONBLOCK, 0)
		if err != nil {
			return err
		}
		defer srcFile.Close()
		var st unix.Stat_t
		if err = unix.Fstat(int(srcFile.Fd()), &st); err != nil {
			return err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG {
			return errors.New("only regular files, directories and symbolic links can be copied")
		}
		dstFile, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		if _, err = io.Copy(dstFile, srcFile); err != nil {
			dstFile.Close()
			return err
		}
		if err = dstFile.Chmod(info.Mode().Perm()); err != nil {
			dstFile.Close()
			return err
		}
		_ = dstFile.Chown(int(st.Uid), int(st.Gid))
		return dstFile.Close()
	case info.IsDir():
		if err := os.Mkdir(dst, 0700); err != nil {
			return err
		}
		children, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, child := range children {
			var cinfo os.FileInfo
			if cinfo, err = child.Info(); err != nil {
				return err
			}
			if err = copyEntry(path.Join(src, child.Name()), path.Join(dst, child.Name()), cinfo); err != nil {
				return err
			}
		}
		// Restore the directory's own mode after filling it, then owner.
		if err = os.Chmod(dst, info.Mode().Perm()); err != nil {
			return err
		}
		if st, ok := info.Sys().(*unix.Stat_t); ok {
			_ = os.Chown(dst, int(st.Uid), int(st.Gid))
		}
		return nil
	default:
		return fmt.Errorf("%s is a special file and cannot be copied", src)
	}
}

// CopyEntry copies src into the existing directory dstDir under its own base
// name, refusing to overwrite an existing entry. Copying a directory into
// itself or its own subtree is rejected before any bytes move.
func CopyEntry(src, dstDir string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() && dirContainsDir(src, dstDir) {
		return errors.New("cannot copy a directory into itself or its own subtree")
	}
	dst := path.Join(dstDir, path.Base(src))
	if _, err = os.Lstat(dst); err == nil {
		return existsErr(dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err = copyEntry(src, dst, info); err != nil {
		_ = os.RemoveAll(dst) // never leave a partial tree behind
		return err
	}
	return nil
}

// MoveEntry moves src into the existing directory dstDir under its own base
// name, refusing to overwrite an existing entry. Same-filesystem moves are a
// plain rename; across filesystems the entry is copied first and the source
// is removed only after the copy fully succeeded.
func MoveEntry(src, dstDir string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.IsDir() && dirContainsDir(src, dstDir) {
		return errors.New("cannot move a directory into itself or its own subtree")
	}
	dst := path.Join(dstDir, path.Base(src))
	if _, err = os.Lstat(dst); err == nil {
		return existsErr(dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err = os.Rename(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}
	// Cross-device: copy, then drop the source only once the copy is complete.
	if err = copyEntry(src, dst, info); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	return os.RemoveAll(src)
}

// newTrashID creates the exclusive per-entry directory inside the bin. The id
// carries the deletion time to keep listing natural and random bytes to keep
// same-instant deletions apart.
func newTrashID() (string, error) {
	var buf [4]byte
	for attempt := 0; ; attempt++ {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		id := fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(buf[:]))
		if err := os.Mkdir(path.Join(TrashDir, id), 0700); err == nil {
			return id, nil
		} else if !errors.Is(err, fs.ErrExist) || attempt >= 4 {
			return "", err
		}
	}
}

// TrashMovePath moves one entry into the recycle bin. The bin lives on the
// root filesystem, so a cross-device deletion copies first and removes the
// original only after the trashed copy and its metadata are durable; any
// earlier failure puts the entry back, so a trash move never loses data.
func TrashMovePath(abs string) error {
	if err := guardFileVirtualTopDir(abs); err != nil {
		return err
	}
	if dirContainsDir(TrashDir, abs) {
		return errors.New("the entry is already in the recycle bin; clear it there instead")
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(TrashDir, 0700); err != nil {
		return err
	}
	id, err := newTrashID()
	if err != nil {
		return err
	}
	// undo removes the half-built trash entry; on the rename path the entry
	// is first moved back, on the copy path the original was never touched.
	item := path.Join(TrashDir, id, trashItemName)
	crossDevice := false
	if err = os.Rename(abs, item); err != nil {
		if !isCrossDevice(err) {
			_ = os.Remove(path.Join(TrashDir, id))
			return err
		}
		crossDevice = true
		if err = copyEntry(abs, item, info); err != nil {
			_ = os.RemoveAll(path.Join(TrashDir, id))
			return err
		}
	}
	if err == nil {
		var meta []byte
		if meta, err = json.Marshal(trashMeta{Original: abs, Name: path.Base(abs), IsDir: info.IsDir(), Deleted: time.Now().Unix()}); err == nil {
			err = os.WriteFile(path.Join(TrashDir, id, trashMetaName), meta, 0600)
		}
	}
	if err != nil {
		if !crossDevice {
			_ = os.Rename(item, abs)
		}
		_ = os.RemoveAll(path.Join(TrashDir, id))
		return err
	}
	if crossDevice {
		err = os.RemoveAll(abs)
		if err != nil {
			// The original could not be removed: roll the copy back out so
			// nothing is stranded inside the bin.
			_ = os.RemoveAll(path.Join(TrashDir, id))
			return err
		}
	}
	return nil
}

// validTrashID accepts only the ids this code creates (nanosecond timestamp,
// dash, lowercase hex), so a crafted id can never climb out of the bin.
func validTrashID(id string) bool {
	stamp, hexPart, ok := strings.Cut(id, "-")
	if !ok || stamp == "" || hexPart == "" || len(id) > 64 {
		return false
	}
	for _, r := range stamp {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range hexPart {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func readTrashMeta(id string) (trashMeta, error) {
	var meta trashMeta
	if !validTrashID(id) {
		return meta, errors.New("invalid recycle-bin entry id")
	}
	data, err := os.ReadFile(path.Join(TrashDir, id, trashMetaName))
	if err != nil {
		return meta, err
	}
	if err = json.Unmarshal(data, &meta); err != nil {
		return meta, fmt.Errorf("corrupt recycle-bin metadata: %w", err)
	}
	return meta, nil
}

// TrashList returns every restorable entry, newest deletion first. Foreign or
// half-written directories inside the bin are skipped rather than breaking
// the whole listing.
func TrashList() ([]TrashItem, error) {
	entries, err := os.ReadDir(TrashDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []TrashItem{}, nil
		}
		return nil, err
	}
	items := make([]TrashItem, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		meta, err := readTrashMeta(id)
		if err != nil {
			continue
		}
		info, err := os.Lstat(path.Join(TrashDir, id, trashItemName))
		if err != nil {
			continue
		}
		items = append(items, TrashItem{ID: id, Name: meta.Name, Original: meta.Original, IsDir: info.IsDir(), Regular: info.Mode().IsRegular(), Symlink: info.Mode()&os.ModeSymlink != 0, Size: info.Size(), Deleted: meta.Deleted})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Deleted > items[j].Deleted })
	return items, nil
}

// TrashRestore puts one trashed entry back at its recorded original location,
// recreating the original directory when it was deleted meanwhile. The
// original location must be free; nothing is ever overwritten.
func TrashRestore(id string) error {
	meta, err := readTrashMeta(id)
	if err != nil {
		return err
	}
	if !ValidAbsPath(meta.Original) || meta.Original == "/" {
		return errors.New("the recorded original path is not a valid absolute path")
	}
	if dirContainsDir(TrashDir, meta.Original) {
		return errors.New("the recorded original path is inside the recycle bin")
	}
	if err = guardFileVirtualTopDir(meta.Original); err != nil {
		return err
	}
	item := path.Join(TrashDir, id, trashItemName)
	if _, err = os.Lstat(item); err != nil {
		return err
	}
	if err = os.MkdirAll(path.Dir(meta.Original), 0750); err != nil {
		return err
	}
	if _, err = os.Lstat(meta.Original); err == nil {
		return existsErr(meta.Original)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err = os.Rename(item, meta.Original); err == nil {
		// Only the per-entry directory with its metadata file remains.
		return os.RemoveAll(path.Join(TrashDir, id))
	} else if !isCrossDevice(err) {
		return err
	}
	// The bin lives on another filesystem than the original location: copy
	// back, then drop the trashed copy.
	info, err := os.Lstat(item)
	if err != nil {
		return err
	}
	if err = copyEntry(item, meta.Original, info); err != nil {
		_ = os.RemoveAll(meta.Original)
		return err
	}
	return os.RemoveAll(path.Join(TrashDir, id))
}

// TrashDelete permanently removes one entry from the bin.
func TrashDelete(id string) error {
	if !validTrashID(id) {
		return errors.New("invalid recycle-bin entry id")
	}
	entry := path.Join(TrashDir, id)
	if _, err := os.Lstat(entry); err != nil {
		return err
	}
	return os.RemoveAll(entry)
}

// TrashEmpty permanently removes every entry from the bin and returns how
// many entries were cleared.
func TrashEmpty() (int, error) {
	entries, err := os.ReadDir(TrashDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	cleared := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if err = os.RemoveAll(path.Join(TrashDir, entry.Name())); err != nil {
			return cleared, err
		}
		cleared++
	}
	return cleared, nil
}
