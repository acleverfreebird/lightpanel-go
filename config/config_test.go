package config

import (
	"golang.org/x/crypto/bcrypt"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigFailsClosed(t *testing.T) {
	t.Setenv("LP_SANDBOX_ROOT", t.TempDir())
	t.Setenv("LP_PASS_HASH", "")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("missing password accepted")
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte("a-long-test-password"), 10)
	t.Setenv("LP_PASS_HASH", string(hash))
	c, err := LoadConfig("")
	if err != nil || c.Host != "127.0.0.1" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	t.Setenv("LP_PORT", "oops")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("invalid port accepted")
	}
	t.Setenv("LP_PORT", "8888")
	t.Setenv("LP_HOST", "0.0.0.0")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("public HTTP allowed")
	}
	t.Setenv("LP_PUBLIC_ORIGIN", "https://panel.example.com")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("public plaintext reverse-proxy backend allowed")
	}
	t.Setenv("LP_HOST", "127.0.0.1")
	if _, err := LoadConfig(""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LP_SANDBOX_ROOT", "/")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("root sandbox allowed")
	}
	t.Setenv("LP_SANDBOX_ROOT", t.TempDir())
	t.Setenv("LP_MAX_UPLOAD_MB", "oops")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("invalid upload limit accepted")
	}
	t.Setenv("LP_MAX_UPLOAD_MB", "2049")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("oversized upload limit accepted")
	}
	t.Setenv("LP_MAX_UPLOAD_MB", "128")
	c, err = LoadConfig("")
	if err != nil || c.MaxUploadMB != 128 {
		t.Fatalf("upload limit: %+v %v", c, err)
	}
	t.Setenv("LP_UPDATE_REPO", "bad")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("invalid update repo accepted")
	}
	t.Setenv("LP_UPDATE_REPO", "acleverfreebird/lightpanel-go")
	t.Setenv("LP_UPDATE_MIRROR", "https://mirror.example.com/")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("mirror with trailing slash accepted")
	}
	t.Setenv("LP_UPDATE_MIRROR", "https://mirror.example.com")
	if _, err := LoadConfig(""); err != nil {
		t.Fatal(err)
	}
}
func TestConfigRejectsUnknownKey(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.toml")
	if err := os.WriteFile(p, []byte("admin_uesr = 'typo'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("unknown field ignored")
	}
}
