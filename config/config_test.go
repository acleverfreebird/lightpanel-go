package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
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
	if err != nil || c.Host != "0.0.0.0" || !c.AllowPublicHTTP {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	t.Setenv("LP_PORT", "oops")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("invalid port accepted")
	}
	t.Setenv("LP_PORT", "8888")
	t.Setenv("LP_HOST", "0.0.0.0")
	// 显式关闭 allow_public_http 后恢复 fail-closed。
	t.Setenv("LP_ALLOW_PUBLIC_HTTP", "false")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("public HTTP allowed after explicit opt-out")
	}
	t.Setenv("LP_PUBLIC_ORIGIN", "https://panel.example.com")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("public plaintext reverse-proxy backend allowed")
	}
	t.Setenv("LP_PUBLIC_ORIGIN", "")
	t.Setenv("LP_ALLOW_PUBLIC_HTTP", "oops")
	if _, err := LoadConfig(""); err == nil {
		t.Fatal("invalid allow_public_http accepted")
	}
	t.Setenv("LP_ALLOW_PUBLIC_HTTP", "true")
	c, err = LoadConfig("")
	if err != nil {
		t.Fatalf("allow_public_http with 0.0.0.0 rejected: %v", err)
	}
	if !c.WildcardOrigin() || c.WildcardScheme() != "http" {
		t.Fatalf("wildcard origin not detected: %+v", c)
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

func TestConfigExplicitPublicHTTPOptOut(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("a-long-test-password"), 10)
	body := `host = "0.0.0.0"` + "\n" +
		`allow_public_http = false` + "\n" +
		`password_hash = "` + string(hash) + `"` + "\n"
	p := filepath.Join(t.TempDir(), "test.toml")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("config-file allow_public_http = false not honored")
	}
}

// TestExampleConfigEnablesAllDefaults 校验「部署即全功能」缺省策略在模板
// （与 install.sh 写入的新装配置同构）上成立：绑定 0.0.0.0、明文 HTTP
// 放行、helper 开关全开、服务控制通配。
func TestExampleConfigEnablesAllDefaults(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("a-long-test-password"), 10)
	body := strings.Replace(exampleConfig, `password_hash = ""`, `password_hash = "`+string(hash)+`"`, 1)
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "0.0.0.0" || !c.AllowPublicHTTP || !c.WildcardOrigin() || c.WildcardScheme() != "http" {
		t.Fatalf("wildcard deployment defaults not applied: %+v", c)
	}
	if c.Helper == nil {
		t.Fatal("helper section lost")
	}
	for name, on := range map[string]bool{
		"allow_firewall": c.Helper.AllowFirewall, "allow_kill": c.Helper.AllowKill,
		"allow_update": c.Helper.AllowUpdate, "allow_sites": c.Helper.AllowSites,
		"allow_apps": c.Helper.AllowApps, "allow_databases": c.Helper.AllowDatabases,
	} {
		if !on {
			t.Errorf("%s not enabled by template", name)
		}
	}
	if acts, ok := c.Helper.Services["*"]; !ok || len(acts) != 3 {
		t.Errorf("services wildcard not enabled: %+v", c.Helper.Services)
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
	if !c.Helper.AllowFirewall || !c.Helper.AllowUpdate {
		t.Errorf("explicit flags lost: %+v", c.Helper)
	}
	// 未显式写入的 allow_* 开关缺省全开（部署即全功能）。
	if !c.Helper.AllowKill || !c.Helper.AllowSites || !c.Helper.AllowApps || !c.Helper.AllowDatabases {
		t.Errorf("absent flags not default-enabled: %+v", c.Helper)
	}
	if len(c.Helper.Services["nginx.service"]) != 3 {
		t.Errorf("services ACL: %+v", c.Helper.Services)
	}

	// 显式写入的值（含 false 与空 services 表）保持管理员决定。
	c, err = LoadConfig(write(t, `
[helper]
allowed_users = ["lightpanel"]
allow_kill = false
allow_sites = false
[helper.services]
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Helper.AllowKill || c.Helper.AllowSites {
		t.Errorf("explicit false overridden: %+v", c.Helper)
	}
	if len(c.Helper.Services) != 0 {
		t.Errorf("empty services table not respected: %+v", c.Helper.Services)
	}
	// [helper] 段整体未写 services 时缺省通配放开所有单元。
	c, err = LoadConfig(write(t, `
[helper]
allowed_users = ["lightpanel"]
`))
	if err != nil {
		t.Fatal(err)
	}
	acts, ok := c.Helper.Services["*"]
	if !ok || len(acts) != 3 {
		t.Errorf("absent services not default-wildcarded: %+v", c.Helper.Services)
	}

	for _, bad := range []string{
		"[helper]\nallowed_users = []\n",                                                               // no users
		"[helper]\nallowed_users = [\"Bad Name\"]\n",                                                   // invalid name
		"[helper]\nsocket = \"run/helper.sock\"\nallowed_users = [\"lp\"]\n",                           // relative socket
		"[helper]\nsocket = \"/run/../helper.sock\"\nallowed_users = [\"lp\"]\n",                       // traversal socket
		"[helper]\nstaging_dir = \"var/tmp\"\nallowed_users = [\"lp\"]\n",                              // relative staging
		"[helper]\nallowed_users = [\"lp\"]\n[helper.services]\n\"nginx\" = [\"start\"]\n",             // not a unit
		"[helper]\nallowed_users = [\"lp\"]\n[helper.services]\n\"nginx.service\" = [\"uninstall\"]\n", // action not whitelisted
	} {
		if _, err := LoadConfig(write(t, bad)); err == nil {
			t.Errorf("invalid helper config accepted:\n%s", bad)
		}
	}
}
