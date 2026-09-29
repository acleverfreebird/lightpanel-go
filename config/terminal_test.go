package config

import (
	"golang.org/x/crypto/bcrypt"
	"os"
	"path/filepath"
	"testing"
)

func TestTerminalDefaultsAndOverrides(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("terminal-config-test"), 10)
	t.Setenv("LP_PASS_HASH", string(hash))
	c, err := LoadConfig("")
	if err != nil || !c.TerminalOn() {
		t.Fatalf("default: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("terminal_enabled = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err = LoadConfig(path)
	if err != nil || c.TerminalOn() {
		t.Fatalf("explicit false: %v", err)
	}
	t.Setenv("LP_TERMINAL_ENABLED", "true")
	c, err = LoadConfig(path)
	if err != nil || !c.TerminalOn() {
		t.Fatalf("env true: %v", err)
	}
	t.Setenv("LP_TERMINAL_ENABLED", "false")
	c, err = LoadConfig("")
	if err != nil || c.TerminalOn() {
		t.Fatalf("env false: %v", err)
	}
	t.Setenv("LP_TERMINAL_ENABLED", "invalid")
	if _, err = LoadConfig(""); err == nil {
		t.Fatal("invalid bool accepted")
	}
}
