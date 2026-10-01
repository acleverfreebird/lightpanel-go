package sysinfo

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func themeForm(body string) *http.Request {
	r := httptest.NewRequest("POST", "/api/theme", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func themeBuiltins() fstest.MapFS {
	return fstest.MapFS{
		"light.css": &fstest.MapFile{Data: []byte("/* name: 浅色\n   scheme: light */\n[data-theme=\"light\"] { --bg: #f5f7f9; --surface: #fff; --accent: #14846e; --text: #24333f; }")},
		"dark.css":  &fstest.MapFile{Data: []byte("/* name: 深色\n   scheme: dark */\n[data-theme=\"dark\"] { --bg: #0f151b; --surface: #151c23; --accent: #2aa487; --text: #d9e2e9; }")},
	}
}

func themeTestManager(t *testing.T) (*ThemeManager, string) {
	t.Helper()
	dir := t.TempDir()
	return NewThemeManagerAt(dir, themeBuiltins()), dir
}

func TestThemeListBuiltinsFirst(t *testing.T) {
	m, _ := themeTestManager(t)
	themes := m.List()
	if len(themes) != 2 {
		t.Fatalf("expected 2 builtin themes, got %d: %+v", len(themes), themes)
	}
	if themes[0].ID != "light" || !themes[0].Builtin {
		t.Fatalf("light must be first builtin, got %+v", themes[0])
	}
	if themes[1].ID != "dark" || themes[1].Scheme != "dark" {
		t.Fatalf("dark theme should carry scheme=dark, got %+v", themes[1])
	}
	if themes[1].URL != "/static/themes/dark.css" {
		t.Fatalf("builtin url should point at embedded static, got %q", themes[1].URL)
	}
	if themes[0].Preview["accent"] != "#14846e" {
		t.Fatalf("preview colors should be extracted, got %+v", themes[0].Preview)
	}
	if themes[0].Name != "浅色" {
		t.Fatalf("display name should come from the CSS header, got %q", themes[0].Name)
	}
}

func TestThemeUploadActivateDelete(t *testing.T) {
	m, dir := themeTestManager(t)

	// 无效 id 直接拒绝（含路径穿越尝试）。
	for _, name := range []string{"../evil", "Big", "a b", "", "a/b"} {
		r := httptest.NewRequest("POST", "/api/theme/upload?name="+url.QueryEscape(name), strings.NewReader("body{}"))
		w := httptest.NewRecorder()
		m.Upload(w, r)
		if w.Code != 400 {
			t.Fatalf("upload with invalid name %q should 400, got %d", name, w.Code)
		}
	}

	// 内置主题不可覆盖。
	r := httptest.NewRequest("POST", "/api/theme/upload?name=dark", strings.NewReader("[data-theme=dark]{}"))
	w := httptest.NewRecorder()
	m.Upload(w, r)
	if w.Code != 403 {
		t.Fatalf("overriding builtin should 403, got %d", w.Code)
	}

	css := "/* name: 夜航 */\n[data-theme=\"night\"] { --bg: #101418; --accent: #7aa2f7; }"
	r = httptest.NewRequest("POST", "/api/theme/upload?name=night&label=%E5%A4%9C%E8%88%AA", strings.NewReader(css))
	w = httptest.NewRecorder()
	m.Upload(w, r)
	if w.Code != 200 {
		t.Fatalf("valid upload should succeed, got %d: %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "night.css")); err != nil {
		t.Fatalf("theme file should be written: %v", err)
	}

	// 激活上传的主题并校验状态持久化。
	r = themeForm("id=night")
	w = httptest.NewRecorder()
	m.Activate(w, r)
	if w.Code != 200 {
		t.Fatalf("activate should succeed, got %d", w.Code)
	}
	data := m.TemplateData()
	if data["ThemeID"] != "night" || data["ThemeCSS"] != "/themes/night" {
		t.Fatalf("template data should follow the active theme, got %+v", data)
	}
	reloaded := NewThemeManagerAt(dir, themeBuiltins())
	if got := reloaded.TemplateData()["ThemeID"]; got != "night" {
		t.Fatalf("active theme should survive restart, got %v", got)
	}

	// 删除激活中的主题回落到 light。
	r = themeForm("id=night")
	w = httptest.NewRecorder()
	m.Delete(w, r)
	if w.Code != 200 {
		t.Fatalf("delete should succeed, got %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(dir, "night.css")); !os.IsNotExist(err) {
		t.Fatalf("theme file should be removed, got %v", err)
	}
	if got := m.TemplateData()["ThemeID"]; got != "light" {
		t.Fatalf("deleting the active theme must fall back to light, got %v", got)
	}
}

func TestThemeUploadValidation(t *testing.T) {
	m, _ := themeTestManager(t)
	cases := []struct {
		name string
		css  string
		want int
	}{
		{"import", "[data-theme=x]{}\n@import url('https://evil.example/x.css');", 400},
		{"script", "[data-theme=x]{} /* </style><script>alert(1)</script> */", 400},
		{"jsurl", "[data-theme=x] { --bg: url(javascript:alert(1)); }", 400},
	}
	for _, tc := range cases {
		r := httptest.NewRequest("POST", "/api/theme/upload?name="+url.QueryEscape(tc.name), strings.NewReader(tc.css))
		w := httptest.NewRecorder()
		m.Upload(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: expected %d, got %d", tc.name, tc.want, w.Code)
		}
	}

	// 超限上传被 MaxBytesReader 截断后应返回 413。
	huge := bytes.Repeat([]byte("a"), MaxThemeUpload+64)
	r := httptest.NewRequest("POST", "/api/theme/upload?name=huge", bytes.NewReader(huge))
	w := httptest.NewRecorder()
	m.Upload(w, r)
	if w.Code != 413 {
		t.Fatalf("oversize upload should 413, got %d", w.Code)
	}
}

func TestThemeServeRejectsTraversal(t *testing.T) {
	m, _ := themeTestManager(t)
	r := httptest.NewRequest("GET", "/themes/light", nil)
	r.SetPathValue("id", "..")
	w := httptest.NewRecorder()
	m.Serve(w, r)
	if w.Code != 400 {
		t.Fatalf("path traversal must be rejected, got %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/themes/missing", nil)
	r.SetPathValue("id", "missing")
	w = httptest.NewRecorder()
	m.Serve(w, r)
	if w.Code != 404 {
		t.Fatalf("missing theme should 404, got %d", w.Code)
	}
}

func TestThemeTemplateDataFallsBackWhenFileRemoved(t *testing.T) {
	m, dir := themeTestManager(t)
	if err := os.WriteFile(filepath.Join(dir, "gone.css"), []byte("[data-theme=gone]{--bg:#000}"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := themeForm("id=gone")
	w := httptest.NewRecorder()
	m.Activate(w, r)
	if w.Code != 200 {
		t.Fatalf("activate custom theme failed: %d", w.Code)
	}
	_ = os.Remove(filepath.Join(dir, "gone.css"))
	if got := m.TemplateData()["ThemeID"]; got != "light" {
		t.Fatalf("missing active theme must fall back to light, got %v", got)
	}
}
