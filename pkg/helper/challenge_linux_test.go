//go:build linux

package helper

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChallengeSetAndClear(t *testing.T) {
	dir := t.TempDir()
	challengeDir = dir
	t.Cleanup(func() { challengeDir = ChallengeDir })

	token := strings.Repeat("a", 43)
	auth := token + "." + strings.Repeat("b", 43)
	if err := challengeSet(token, auth); err != nil {
		t.Fatalf("set: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, token))
	if err != nil || string(data) != auth {
		t.Fatalf("challenge file: %q %v", data, err)
	}
	// Directory must be worker-readable, the file world-readable.
	if fi, _ := os.Stat(dir); fi.Mode().Perm()&0o555 != 0o555 {
		t.Fatalf("challenge dir perms %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Join(dir, token)); fi.Mode().Perm() != 0o644 {
		t.Fatalf("challenge file perms %v", fi.Mode().Perm())
	}
	if err := challengeClear(token); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, token)); !os.IsNotExist(err) {
		t.Fatalf("challenge file survived: %v", err)
	}
	// Janitor mode: empty token clears every file.
	for i := 0; i < 3; i++ {
		if err := challengeSet(strings.Repeat(string(rune('a'+i)), 43), strings.Repeat("z", 43)+"."+strings.Repeat("y", 43)); err != nil {
			t.Fatal(err)
		}
	}
	if err := challengeClear(""); err != nil {
		t.Fatalf("janitor clear: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("directory not emptied: %v %d", err, len(entries))
	}
}

func TestChallengeValidation(t *testing.T) {
	challengeDir = t.TempDir()
	t.Cleanup(func() { challengeDir = ChallengeDir })
	good := strings.Repeat("a", 43)
	for _, c := range []struct{ token, auth string }{
		{"", good + "." + good},
		{strings.Repeat("a", 42), strings.Repeat("a", 42) + "." + good}, // too short
		{strings.Repeat("a", 44), strings.Repeat("a", 44) + "." + good}, // too long
		{good + "!", good + "!." + good},                                // bad alphabet
		{good, good},                                                    // missing thumbprint
		{good, good + ".." + good},                                      // malformed
		{good, good + "." + strings.Repeat("b", 42)},                    // short thumbprint
	} {
		if err := challengeSet(c.token, c.auth); err == nil {
			t.Errorf("challengeSet(%q, %q) accepted", c.token, c.auth)
		}
	}
	if err := challengeClear(good); err != nil {
		// clearing a nonexistent (but valid) token is a no-op
		t.Errorf("clear of unknown token failed: %v", err)
	}
	if err := challengeClear("short"); err == nil {
		t.Error("clear accepted invalid token")
	}
}
