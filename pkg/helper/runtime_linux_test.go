//go:build linux

package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRejectsOversizedBinary(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "update"), filepath.Join(dir, "panel")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(updateExeMax + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err = os.WriteFile(dst, []byte("original"), 0755); err != nil {
		t.Fatal(err)
	}
	if err = installFile(src, dst); err == nil {
		t.Fatal("oversized update accepted")
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "original" {
		t.Fatal("original binary replaced")
	}
}

func TestBoundedCommandOutput(t *testing.T) {
	var b commandBuffer
	data := []byte(strings.Repeat("x", 700000))
	for i := 0; i < 4; i++ {
		n, err := b.Write(data)
		if n != len(data) || err != nil {
			t.Fatal(n, err)
		}
	}
	if b.Len() != 1<<20 || !b.truncated {
		t.Fatalf("len=%d truncated=%v", b.Len(), b.truncated)
	}
}
