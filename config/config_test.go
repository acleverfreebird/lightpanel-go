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
func TestConfigAcceptsDeprecatedSandboxRoot(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.toml")
	hash, _ := bcrypt.GenerateFromPassword([]byte("a-long-test-password"), 10)
	body := "password_hash = \"" + string(hash) + "\"\nsandbox_root = \"/var/lib/lightpanel/files\"\n"
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err != nil {
		t.Fatalf("deprecated sandbox_root rejected: %v", err)
	}
}

func TestConfigHelperSection(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("a-long-test-password"), 10)
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "test.toml")
		full := "password_hash = \"" + string(hash) + "\"\n" + body
		if err := os.WriteFile(p, []byte(full), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := write(t, `
[helper]
allowed_users = ["lightpanel"]
allow_firewall = true
allow_update = true
[helper.services]
"nginx.service" = ["start", "stop", "restart"]
"*" = []
`)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Helper == nil {
		t.Fatal("helper section lost")
	}
	if c.Helper.Socket != "/run/lightpanel/helper.sock" {
		t.Errorf("socket default not applied: %q", c.Helper.Socket)
	}
	if c.Helper.StagingDir != "/var/lib/lightpanel/update" {
		t.Errorf("staging_dir default not applied: %q", c.Helper.StagingDir)
	}
	if !c.Helper.AllowFirewall || c.Helper.AllowKill {
		t.Errorf("flags: %+v", c.Helper)
	}
	if len(c.Helper.Services["nginx.service"]) != 3 {
		t.Errorf("services ACL: %+v", c.Helper.Services)
	}

	for _, bad := range []string{
		"[helper]\nallowed_users = []\n",                                                            // no users
		"[helper]\nallowed_users = [\"Bad Name\"]\n",                                                // invalid name
		"[helper]\nsocket = \"run/helper.sock\"\nallowed_users = [\"lp\"]\n",                        // relative socket
		"[helper]\nsocket = \"/run/../helper.sock\"\nallowed_users = [\"lp\"]\n",                    // traversal socket
		"[helper]\nstaging_dir = \"var/tmp\"\nallowed_users = [\"lp\"]\n",                           // relative staging
		"[helper]\nallowed_users = [\"lp\"]\n[helper.services]\n\"nginx\" = [\"start\"]\n",          // not a unit
		"[helper]\nallowed_users = [\"lp\"]\n[helper.services]\n\"nginx.service\" = [\"enable\"]\n", // action not whitelisted
	} {
		if _, err := LoadConfig(write(t, bad)); err == nil {
			t.Errorf("invalid helper config accepted:\n%s", bad)
		}
	}
}
