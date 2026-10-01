package sysinfo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// 面板主题：一个主题就是一个 CSS 文件，通过 [data-theme="<id>"] 选择器覆盖
// app.css :root 里的设计变量。内置主题随二进制嵌入（static/themes/*.css），
// 用户主题上传后保存在面板状态目录，经 /themes/{id} 提供服务。激活的主题
// 由服务端渲染进 <head>（body 的 data-theme 属性 + <link>），因此切换主题
// 全站即时生效、刷新无闪烁，也不依赖 localStorage。

const (
	MaxThemeUpload = 256 << 10
	maxThemeFiles  = 64
)

// themeStateVersion 用于将来扩展状态文件的字段。
const themeStateVersion = 1

// 主题 id 同时用作文件名：只允许小写 slug，杜绝路径穿越与大小写歧义。
var themeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// 预览色板：从 CSS 里提取最核心的四个变量，供主题卡片渲染色块。
var themeVarPattern = regexp.MustCompile(`--(bg|surface|accent|text)\s*:\s*([^;}]+)`)
var themeNamePattern = regexp.MustCompile(`(?m)^\s*(?:/\*)?\s*name:\s*(.+?)\s*(?:\*/)?\s*$`)
var themeSchemePattern = regexp.MustCompile(`(?m)^\s*(?:/\*)?\s*scheme:\s*([a-zA-Z]+)`)
var themeSwatchPattern = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)

// 上传内容黑名单：主题应该是纯变量覆盖；CSP 已禁止外部资源加载，这里再做
// 一层纵深防御，拒绝 @import、脚本注入等表达式。
var themeForbidden = []string{"@import", "javascript:", "expression(", "<script", "</style"}

type Theme struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Builtin bool              `json:"builtin"`
	URL     string            `json:"url"`
	Scheme  string            `json:"scheme,omitempty"`
	Size    int64             `json:"size"`
	Updated time.Time         `json:"updated,omitempty"`
	Preview map[string]string `json:"preview,omitempty"`
}

type themeState struct {
	Version int    `json:"version"`
	Active  string `json:"active"`
}

type ThemeManager struct {
	mu       sync.Mutex
	dir      string
	builtins fs.FS
	active   string
}

// NewThemeManager 使用与凭据存储一致的状态目录解析规则：root 面板写
// /var/lib/lightpanel，否则退回用户 XDG state 目录。
func NewThemeManager(builtins fs.FS) *ThemeManager {
	dir := "/var/lib/lightpanel/themes"
	if !writableDir(dir) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			dir = filepath.Join(home, ".local", "state", "lightpanel", "themes")
		}
	}
	return NewThemeManagerAt(dir, builtins)
}

// NewThemeManagerAt 允许指定目录（测试用）。
func NewThemeManagerAt(dir string, builtins fs.FS) *ThemeManager {
	m := &ThemeManager{dir: dir, builtins: builtins, active: "light"}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return m
	}
	if data, err := os.ReadFile(m.statePath()); err == nil {
		var state themeState
		if json.Unmarshal(data, &state) == nil && state.Version == themeStateVersion && state.Active != "" {
			m.active = state.Active
		}
	}
	return m
}

func (m *ThemeManager) statePath() string { return filepath.Join(m.dir, "state.json") }
func (m *ThemeManager) filePath(id string) string {
	return filepath.Join(m.dir, id+".css")
}

// parseThemeCSS 读取主题 CSS 并提取展示名、color-scheme 与预览色。
func parseThemeCSS(id string, data []byte, builtin bool) (Theme, error) {
	if !utf8.Valid(data) {
		return Theme{}, errors.New("主题文件必须是 UTF-8 文本")
	}
	text := string(data)
	lower := strings.ToLower(text)
	for _, bad := range themeForbidden {
		if strings.Contains(lower, bad) {
			return Theme{}, fmt.Errorf("主题文件包含不允许的内容（%s）", bad)
		}
	}
	name, scheme := "", "light"
	if hit := themeNamePattern.FindStringSubmatch(text); hit != nil {
		name = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(hit[1]), "*/"))
	}
	if hit := themeSchemePattern.FindStringSubmatch(text); hit != nil && hit[1] == "dark" {
		scheme = "dark"
	}
	preview := map[string]string{}
	for _, hit := range themeVarPattern.FindAllStringSubmatch(text, -1) {
		value := strings.TrimSpace(hit[2])
		if themeSwatchPattern.MatchString(value) {
			preview[hit[1]] = value
		}
	}
	url := "/themes/" + id
	if builtin {
		url = "/static/themes/" + id + ".css"
	}
	return Theme{ID: id, Name: name, Builtin: builtin, URL: url, Scheme: scheme, Preview: preview}, nil
}

// list 返回全部主题：内置在前（light 永远第一），用户主题按 id 排序。
func (m *ThemeManager) list() []Theme {
	var themes []Theme
	if m.builtins != nil {
		entries, err := fs.ReadDir(m.builtins, ".")
		if err == nil {
			for _, entry := range entries {
				id := strings.TrimSuffix(entry.Name(), ".css")
				if filepath.Ext(entry.Name()) != ".css" || !themeIDPattern.MatchString(id) {
					continue
				}
				data, err := fs.ReadFile(m.builtins, entry.Name())
				if err != nil {
					continue
				}
				theme, err := parseThemeCSS(id, data, true)
				if err != nil {
					continue
				}
				theme.Size = int64(len(data))
				themes = append(themes, theme)
			}
		}
	}
	// light 是 app.css :root 的基础配色，排在第一位作为重置选项。
	sort.Slice(themes, func(i, j int) bool {
		if (themes[i].ID == "light") != (themes[j].ID == "light") {
			return themes[i].ID == "light"
		}
		return themes[i].ID < themes[j].ID
	})
	if entries, err := os.ReadDir(m.dir); err == nil {
		var ids []string
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) == ".css" {
				ids = append(ids, strings.TrimSuffix(entry.Name(), ".css"))
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			if theme, err := m.loadUser(id); err == nil {
				themes = append(themes, theme)
			}
		}
	}
	return themes
}

func (m *ThemeManager) loadUser(id string) (Theme, error) {
	if !themeIDPattern.MatchString(id) {
		return Theme{}, errors.New("invalid theme id")
	}
	data, err := os.ReadFile(m.filePath(id))
	if err != nil {
		return Theme{}, err
	}
	theme, err := parseThemeCSS(id, data, false)
	if err != nil {
		return Theme{}, err
	}
	theme.Size = int64(len(data))
	theme.Updated = fileTime(m.filePath(id))
	return theme, nil
}

// List 是给 API 用的快照。
func (m *ThemeManager) List() []Theme { return m.list() }

func (m *ThemeManager) exists(id string) (Theme, bool) {
	for _, theme := range m.list() {
		if theme.ID == id {
			return theme, true
		}
	}
	return Theme{}, false
}

// TemplateData 是模板渲染用的激活主题数据；激活的主题文件被外部删除时
// 自动回落到 light，避免渲染出指向缺失文件的 <link>。
func (m *ThemeManager) TemplateData() map[string]any {
	m.mu.Lock()
	active := m.active
	m.mu.Unlock()
	var chosen Theme
	for _, theme := range m.list() {
		if theme.ID == active {
			chosen = theme
			break
		}
	}
	if chosen.ID == "" {
		if active != "light" {
			m.mu.Lock()
			m.active = "light"
			m.mu.Unlock()
			_ = m.saveActive("light")
		}
		chosen, _ = m.exists("light")
	}
	return map[string]any{"ThemeID": chosen.ID, "ThemeCSS": chosen.URL, "Scheme": chosen.Scheme}
}

// saveActive 原子写入状态文件：同目录临时文件 0600 后 rename。
func (m *ThemeManager) saveActive(id string) error {
	data, err := json.Marshal(themeState{Version: themeStateVersion, Active: id})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.dir, ".theme-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(data)
	if closeErr := tmp.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		os.Remove(tmpName)
		return writeErr
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, m.statePath())
}

func (m *ThemeManager) builtinIDs() map[string]bool {
	ids := map[string]bool{}
	if m.builtins == nil {
		return ids
	}
	if entries, err := fs.ReadDir(m.builtins, "."); err == nil {
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) == ".css" {
				ids[strings.TrimSuffix(entry.Name(), ".css")] = true
			}
		}
	}
	return ids
}

// Activate 切换激活主题并持久化，返回前端热应用所需的数据。
func (m *ThemeManager) Activate(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.FormValue("id"))
	theme, ok := m.exists(id)
	if !ok {
		http.Error(w, "theme not found", 404)
		return
	}
	m.mu.Lock()
	m.active = id
	m.mu.Unlock()
	if err := m.saveActive(id); err != nil {
		http.Error(w, "failed to save theme state", 500)
		return
	}
	JSON(w, map[string]any{"ok": true, "id": theme.ID, "name": theme.Name, "url": theme.URL, "scheme": theme.Scheme})
}

// Upload 接收原始 CSS 正文（与文件上传一致：body 即文件内容），query 携带
// slug 化的 name 与展示用 label。同名重复上传视为更新；内置主题不可覆盖。
func (m *ThemeManager) Upload(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if !themeIDPattern.MatchString(name) {
		http.Error(w, "主题名称只能包含小写字母、数字、中划线和下划线，并以字母或数字开头", 400)
		return
	}
	if _, builtin := m.builtinIDs()[name]; builtin {
		http.Error(w, "内置主题不能被覆盖，请换一个名称", 403)
		return
	}
	body := http.MaxBytesReader(w, r.Body, MaxThemeUpload+1)
	data, err := io.ReadAll(body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, fmt.Sprintf("主题文件超过 %d KiB 上限", MaxThemeUpload>>10), 413)
		} else {
			http.Error(w, "failed to read upload", 400)
		}
		return
	}
	theme, err := parseThemeCSS(name, data, false)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if theme.Name == "" {
		// CSS 没有 name: 头时退回上传文件名（去掉控制字符，截断到 48 字符）。
		label := strings.Map(func(r rune) rune {
			if r < 32 || r == 0x7f {
				return -1
			}
			return r
		}, strings.TrimSpace(r.URL.Query().Get("label")))
		if label != "" {
			if len(label) > 48 {
				label = string([]rune(label)[:48])
			}
			theme.Name = label
		}
	}
	if theme.Name == "" {
		theme.Name = name
	}
	if m.userCount() >= maxThemeFiles {
		if _, err := os.Stat(m.filePath(name)); err != nil {
			http.Error(w, "主题数量已达上限，请先删除不需要的主题", 400)
			return
		}
	}
	if err := os.WriteFile(m.filePath(name), data, 0o600); err != nil {
		http.Error(w, "failed to save theme", 500)
		return
	}
	theme.Updated = fileTime(m.filePath(name))
	JSON(w, map[string]any{"ok": true, "theme": theme})
}

func (m *ThemeManager) userCount() int {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".css" {
			count++
		}
	}
	return count
}

// Delete 删除用户主题；删掉激活主题时回落到 light。
func (m *ThemeManager) Delete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.FormValue("id"))
	if _, builtin := m.builtinIDs()[id]; builtin {
		http.Error(w, "内置主题不能删除", 403)
		return
	}
	if _, err := os.Stat(m.filePath(id)); err != nil {
		http.Error(w, "theme not found", 404)
		return
	}
	if err := os.Remove(m.filePath(id)); err != nil {
		http.Error(w, "failed to delete theme", 500)
		return
	}
	m.mu.Lock()
	active := m.active
	if active == id {
		m.active = "light"
	}
	m.mu.Unlock()
	if active == id {
		_ = m.saveActive("light")
	}
	JSON(w, map[string]any{"ok": true, "active": "light"})
}

// Serve 提供用户主题 CSS。id 已通过 slug 校验，不存在路径穿越的可能。
func (m *ThemeManager) Serve(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !themeIDPattern.MatchString(id) {
		http.Error(w, "invalid theme", 400)
		return
	}
	path := m.filePath(id)
	info, err := os.Stat(path)
	if err != nil {
		http.Error(w, "theme not found", 404)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "failed to read theme", 500)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	_, _ = w.Write(data)
}

func fileTime(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}
