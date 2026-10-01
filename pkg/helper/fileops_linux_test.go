//go:build linux

package helper

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func writeFileTree(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0640); err != nil {
		t.Fatal(err)
	}
}

func TestCopyEntryFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	writeFileTree(t, src, "hello")
	dst := filepath.Join(dir, "out")
	if err := os.Mkdir(dst, 0755); err != nil {
		t.Fatal(err)
	}
	if err := CopyEntry(src, dst); err != nil {
		t.Fatalf("copy: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("copied content %q %v", data, err)
	}
	info, err := os.Stat(filepath.Join(dst, "a.txt"))
	if err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("copied mode %v %v", info, err)
	}
	// No-clobber: an existing entry is never replaced.
	if err := CopyEntry(src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("copy over existing entry: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(data) != "hello" {
		t.Fatal("existing entry was overwritten")
	}
}

func TestCopyEntryTree(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0750); err != nil {
		t.Fatal(err)
	}
	writeFileTree(t, filepath.Join(src, "top.txt"), "top")
	writeFileTree(t, filepath.Join(src, "sub", "inner.txt"), "inner")
	if err := os.Symlink("top.txt", filepath.Join(src, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0755); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "out")
	if err := os.Mkdir(dst, 0755); err != nil {
		t.Fatal(err)
	}
	if err := CopyEntry(src, dst); err != nil {
		t.Fatalf("copy tree: %v", err)
	}
	for path, want := range map[string]string{
		filepath.Join(dst, "tree", "top.txt"):          "top",
		filepath.Join(dst, "tree", "sub", "inner.txt"): "inner",
	} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("%s = %q %v", path, data, err)
		}
	}
	info, err := os.Lstat(filepath.Join(dst, "tree", "alias"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink not preserved: %v %v", info, err)
	}
	dirInfo, err := os.Stat(filepath.Join(dst, "tree", "sub"))
	if err != nil || dirInfo.Mode().Perm() != 0750 {
		t.Fatalf("directory mode not preserved: %v %v", dirInfo, err)
	}
	// A directory is never copied into itself or its own subtree.
	if err := CopyEntry(src, filepath.Join(src, "sub")); err == nil {
		t.Fatal("copy into own subtree accepted")
	}
}

func TestMoveEntry(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFileTree(t, filepath.Join(src, "sub", "x.txt"), "x")
	writeFileTree(t, filepath.Join(dir, "flat.txt"), "flat")
	dest := filepath.Join(dir, "dest")
	if err := os.Mkdir(dest, 0755); err != nil {
		t.Fatal(err)
	}
	if err := MoveEntry(filepath.Join(dir, "flat.txt"), dest); err != nil {
		t.Fatalf("move file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "flat.txt")); err != nil {
		t.Fatalf("moved file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "flat.txt")); !os.IsNotExist(err) {
		t.Fatal("source survived the move")
	}
	if err := MoveEntry(src, dest); err != nil {
		t.Fatalf("move tree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "tree", "sub", "x.txt")); err != nil {
		t.Fatalf("moved tree broken: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("source tree survived the move")
	}
	// No-clobber, and never into the moved directory's own subtree.
	if err := MoveEntry(filepath.Join(dest, "tree"), dest); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("move over existing entry: %v", err)
	}
	if err := MoveEntry(filepath.Join(dest, "tree"), filepath.Join(dest, "tree", "sub")); err == nil {
		t.Fatal("move into own subtree accepted")
	}
}

func withTestTrash(t *testing.T) {
	t.Helper()
	saved := TrashDir
	TrashDir = filepath.Join(t.TempDir(), "trash")
	t.Cleanup(func() { TrashDir = saved })
}

func TestTrashLifecycle(t *testing.T) {
	withTestTrash(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	writeFileTree(t, src, "trash me")
	if err := TrashMovePath(src); err != nil {
		t.Fatalf("trash move: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("original survived the trash move")
	}
	items, err := TrashList()
	if err != nil || len(items) != 1 {
		t.Fatalf("trash list %v %+v", err, items)
	}
	item := items[0]
	if item.Name != "a.txt" || item.Original != src || item.Deleted <= 0 || item.Regular != true {
		t.Fatalf("trash item %+v", item)
	}
	// Restoring onto a live original is refused without touching either side.
	writeFileTree(t, src, "conflict")
	if err := TrashRestore(item.ID); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("restore over existing entry: %v", err)
	}
	if data, _ := os.ReadFile(src); string(data) != "conflict" {
		t.Fatal("conflicting original was overwritten")
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	if err := TrashRestore(item.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if data, err := os.ReadFile(src); err != nil || string(data) != "trash me" {
		t.Fatalf("restored content %q %v", data, err)
	}
	if items, _ = TrashList(); len(items) != 0 {
		t.Fatalf("restored entry still listed: %+v", items)
	}
}

func TestTrashDirectoryEntry(t *testing.T) {
	withTestTrash(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFileTree(t, filepath.Join(src, "sub", "x.txt"), "x")
	if err := TrashMovePath(src); err != nil {
		t.Fatalf("trash dir: %v", err)
	}
	items, err := TrashList()
	if err != nil || len(items) != 1 || !items[0].IsDir {
		t.Fatalf("trash list %v %+v", err, items)
	}
	// Permanent delete clears the whole entry.
	if err = TrashDelete(items[0].ID); err != nil {
		t.Fatalf("trash delete: %v", err)
	}
	if items, _ = TrashList(); len(items) != 0 {
		t.Fatal("deleted entry still listed")
	}
}

func TestTrashEmptyAndGuards(t *testing.T) {
	withTestTrash(t)
	dir := t.TempDir()
	writeFileTree(t, filepath.Join(dir, "one.txt"), "1")
	if err := os.Mkdir(filepath.Join(dir, "d"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := TrashMovePath(filepath.Join(dir, "one.txt")); err != nil {
		t.Fatal(err)
	}
	if err := TrashMovePath(filepath.Join(dir, "d")); err != nil {
		t.Fatal(err)
	}
	cleared, err := TrashEmpty()
	if err != nil || cleared != 2 {
		t.Fatalf("trash empty %d %v", cleared, err)
	}
	if items, _ := TrashList(); len(items) != 0 {
		t.Fatal("bin not empty after clearing")
	}
	// Virtual system directories and the bin itself are refused.
	if err := TrashMovePath("/proc"); err == nil {
		t.Fatal("trashing /proc accepted")
	}
	if err := os.MkdirAll(filepath.Join(TrashDir, "nesting"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := TrashMovePath(filepath.Join(TrashDir, "nesting")); err == nil {
		t.Fatal("trashing a path inside the bin accepted")
	}
	// Ids are validated against the id alphabet before touching the disk.
	if err := TrashRestore("../escape"); err == nil {
		t.Fatal("path-shaped id accepted")
	}
	if err := TrashRestore("123-zzzz"); err == nil {
		t.Fatal("non-hex id accepted")
	}
	if err := TrashDelete("123-abcdef01"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unknown id: %v", err)
	}
}
