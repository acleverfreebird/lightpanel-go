package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

//go:embed example.toml
var exampleConfig string

var (
	sectionRe = regexp.MustCompile(`^\[([A-Za-z0-9_.-]+)\]$`)
	keyRe     = regexp.MustCompile(`^([A-Za-z0-9_.-]+) *=`)
	// 可选键：模板中以 `# key = value` 注释形式书写的条目（# 后恰一个空格、
	// 键不带引号）。补全时保持注释形式，是否启用由管理员取消注释决定——
	// 升级迁移绝不静默开启特权授权。缩进的示例行（如 services 表注释）不匹配。
	optionalKeyRe = regexp.MustCompile(`^# ([A-Za-z0-9_.-]+) *=`)
)

// configEntry 是模板/配置文件中的一个可迁移条目：段头或键行，连同其前导
// 注释块原样保留。带引号的键（如 services 表条目）与无法识别的行不构成
// 条目，不会被补全。
type configEntry struct {
	section   string   // 所属段；"" = 顶层
	key       string   // 段头条目为段名
	lines     []string // 前导注释 + 本行
	isSection bool
}

// parseEntries 按行扫描 TOML 文本，抽取条目序列。注释块附着到紧随其后的
// 条目；空行切断注释块。这不是完整的 TOML 解析器——多行值等语法不在配置
// 模板中使用，未识别的行只影响注释归属，不会被补全。
func parseEntries(text string) []configEntry {
	var entries []configEntry
	var comments []string
	section := ""
	add := func(parent string, key string, isSection bool, line string) {
		lines := make([]string, 0, len(comments)+1)
		lines = append(lines, comments...)
		lines = append(lines, line)
		entries = append(entries, configEntry{section: parent, key: key, lines: lines, isSection: isSection})
		comments = nil
	}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			comments = nil
		case strings.HasPrefix(t, "#"):
			if m := optionalKeyRe.FindStringSubmatch(t); m != nil {
				add(section, m[1], false, line)
			} else {
				comments = append(comments, line)
			}
		default:
			if m := sectionRe.FindStringSubmatch(t); m != nil {
				parent := ""
				if i := strings.LastIndexByte(m[1], '.'); i >= 0 {
					parent = m[1][:i]
				}
				parentOf := parent
				section = m[1]
				add(parentOf, m[1], true, line)
			} else if m := keyRe.FindStringSubmatch(t); m != nil {
				add(section, m[1], false, line)
			} else {
				comments = append(comments, line)
			}
		}
	}
	return entries
}

// EnsureCurrent 把模板（example.toml）中用户配置缺失的条目补写进 path，
// 供版本升级后自动同步新增配置项。已有键的值与注释一律保留；可选键
// （特权授权等）以注释形式补全。变更前先写 path.bak 备份，再经同目录
// 临时文件原子替换，替换前校验合并结果可被 TOML 解析。
//
// 面板与 helper 进程都会在启动时执行迁移：两者基于同一模板与同一文件，
// 生成内容一致，并发时后写覆盖前写，结果幂等。文件不存在时（全新安装由
// install.sh 写入完整配置）不做任何事。
func EnsureCurrent(path string) (added []string, changed bool, err error) {
	if path == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	userKeys := map[string]bool{}
	for _, e := range parseEntries(string(data)) {
		userKeys[e.section+"\x00"+e.key] = true
	}
	var missing []configEntry
	for _, e := range parseEntries(exampleConfig) {
		if !userKeys[e.section+"\x00"+e.key] {
			missing = append(missing, e)
			name := e.key
			if e.section != "" {
				name = e.section + "." + e.key
			}
			added = append(added, name)
		}
	}
	if len(missing) == 0 {
		return nil, false, nil
	}

	lines := splitLines(string(data))
	firstSection := -1
	sectionHeader := map[string]int{} // 段名 -> 段头行号
	for i, line := range lines {
		if m := sectionRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if _, ok := sectionHeader[m[1]]; !ok {
				sectionHeader[m[1]] = i
			}
			if firstSection < 0 {
				firstSection = i
			}
		}
	}
	endOfSection := func(header int) int {
		for j := header + 1; j < len(lines); j++ {
			if sectionRe.MatchString(strings.TrimSpace(lines[j])) {
				return j
			}
		}
		return len(lines)
	}

	missingSection := map[string]bool{}
	for _, e := range missing {
		if e.isSection {
			missingSection[e.key] = true
		}
	}
	type insert struct {
		at    int
		lines []string
	}
	var inserts []insert
	var tail []string // 缺失整段（及段内条目）追加到文件尾
	// run 把一段连续缺失条目合并为块，块尾留一个空行与后续内容分隔。
	run := func(entries []configEntry) []string {
		block := make([]string, 0, 8)
		for _, e := range entries {
			block = append(block, e.lines...)
		}
		return append(block, "")
	}
	for i := 0; i < len(missing); {
		e := missing[i]
		if e.isSection && missingSection[e.key] {
			// 整段缺失：段头连同后续属于该段的缺失条目一起追加到文件尾。
			j := i + 1
			for j < len(missing) && missing[j].section == e.key {
				j++
			}
			tail = append(tail, run(missing[i:j])...)
			i = j
			continue
		}
		j := i + 1
		for j < len(missing) && !missing[j].isSection && missing[j].section == e.section {
			j++
		}
		at := -1
		if e.section == "" {
			at = firstSection
			if at < 0 {
				at = len(lines)
			}
		} else if header, ok := sectionHeader[e.section]; ok {
			at = endOfSection(header)
		} else {
			// 父段缺失但段头不在缺失列表中：理论上不可达（段头是普通
			// 条目，父段缺失时必然一同缺失），兜底追加到文件尾。
		}
		if at < 0 {
			tail = append(tail, run(missing[i:j])...)
		} else {
			inserts = append(inserts, insert{at: at, lines: run(missing[i:j])})
		}
		i = j
	}
	for i := len(inserts) - 1; i >= 0; i-- { // 自后向前插入避免行号失效
		ins := inserts[i]
		lines = append(lines[:ins.at], append(ins.lines, lines[ins.at:]...)...)
	}
	lines = append(lines, tail...)

	merged := strings.Join(lines, "\n") + "\n"
	var sanity map[string]any
	if err := toml.Unmarshal([]byte(merged), &sanity); err != nil {
		return added, false, fmt.Errorf("merged config does not parse: %w", err)
	}
	if err := writeAtomic(path, data, []byte(merged)); err != nil {
		return added, false, err
	}
	return added, true, nil
}

// splitLines 去掉结尾换行产生的空元素，重组时统一补回单个结尾换行。
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// writeAtomic 先备份原内容到 path.bak，再经同目录临时文件写入并 rename，
// 权限沿用原文件。中途任何失败都保留原配置文件不动。
func writeAtomic(path string, original, merged []byte) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.WriteFile(path+".bak", original, mode); err != nil {
		return fmt.Errorf("write backup %s.bak: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lightpanel-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(merged); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
