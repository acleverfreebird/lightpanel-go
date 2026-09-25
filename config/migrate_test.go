package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// oldConfig 模拟一个旧版本安装生成的配置：没有 [helper] 段，也没有后来
// 新增的顶层键（如 update_repo）。
const oldConfig = `host = "127.0.0.1"
port = 9999
admin_user = "bob"
password_hash = "$2a$10$example"
`

func TestEnsureCurrentFillsMissingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(oldConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	added, changed, err := EnsureCurrent(path)
	if err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	if !changed {
		t.Fatal("expected a change")
	}
	joined := strings.Join(added, ",")
	for _, want := range []string{"update_repo", "helper", "helper.services"} {
		if !strings.Contains(joined, want) {
			t.Errorf("added %q misses %q", joined, want)
		}
	}
	merged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(merged)
	// 已有键的值与位置保持原样。
	for _, want := range []string{`port = 9999`, `admin_user = "bob"`, `password_hash = "$2a$10$example"`} {
		if !strings.Contains(text, want) {
			t.Errorf("existing value lost: %q missing from\n%s", want, text)
		}
	}
	// 特权授权开关以注释形式补全，绝不静默开启。
	for _, key := range []string{"allow_apps", "allow_databases", "allow_firewall"} {
		line := findLine(text, key+" = ")
		if line == "" {
			t.Errorf("%s not filled in:\n%s", key, text)
		} else if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("%s was enabled silently: %q", key, line)
		}
	}
	// 段与键都在，且全部键名对新版 Config 可见（DisallowUnknownFields 语义）。
	var c Config
	dec := toml.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		t.Errorf("merged config rejected by decoder: %v\n%s", err, text)
	}
	if c.Helper == nil || c.Port != 9999 {
		t.Errorf("unexpected parse result: %+v", c)
	}
	// [helper.services] 段补全为带说明注释的空段，条目由管理员按需添加。
	if !strings.Contains(text, "[helper.services]") {
		t.Errorf("[helper.services] header missing:\n%s", text)
	}
	// 备份保存的是迁移前的内容。
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if string(backup) != oldConfig {
		t.Errorf("backup content mismatch:\n%s", backup)
	}
	// 迁移后的文件权限沿用原文件（Windows 不保留 Unix 权限位，跳过）。
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v, err = %v", st, err)
		}
	}
}

func TestEnsureCurrentIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(oldConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := EnsureCurrent(path); err != nil || !changed {
		t.Fatalf("first run: changed=%v err=%v", changed, err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, changed, err := EnsureCurrent(path)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if changed {
		t.Error("second run changed the file again")
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("file changed on the second run")
	}
}

func TestEnsureCurrentTreatsCommentedOptionalKeysAsPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// 管理员已按模板注释写好可选键（尚未启用）：不应重复插入。
	content := oldConfig + "\n[helper]\nallowed_users = [\"lightpanel\"]\n# allow_apps = true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	added, changed, err := EnsureCurrent(path)
	if err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	if !changed {
		t.Fatal("expected other missing keys to be filled")
	}
	for _, a := range added {
		if a == "allow_apps" || strings.HasSuffix(a, ".allow_apps") {
			t.Errorf("commented optional key re-inserted: %q", a)
		}
	}
	text, _ := os.ReadFile(path)
	if n := strings.Count(string(text), "allow_apps"); n != 1 {
		t.Errorf("allow_apps appears %d times, want 1:\n%s", n, text)
	}
}

func TestEnsureCurrentNoopWhenComplete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// 模板自身自然是完整的。
	if err := os.WriteFile(path, []byte(exampleConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed, err := EnsureCurrent(path)
	if err != nil {
		t.Fatalf("EnsureCurrent: %v", err)
	}
	if changed {
		t.Error("complete config was rewritten")
	}
	if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
		t.Error("backup written despite no change")
	}
}

func TestEnsureCurrentSkipsMissingAndEmptyPath(t *testing.T) {
	if added, changed, err := EnsureCurrent(""); len(added) > 0 || changed || err != nil {
		t.Errorf("empty path: added=%v changed=%v err=%v", added, changed, err)
	}
	_, changed, err := EnsureCurrent(filepath.Join(t.TempDir(), "absent.toml"))
	if changed || err != nil {
		t.Errorf("missing file: changed=%v err=%v", changed, err)
	}
}

func findLine(text, substr string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}
