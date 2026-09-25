//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"lightpanel/config"
)

type helperDiagnostics struct {
	Configured       bool `json:"configured"`
	Reachable        bool `json:"reachable"`
	ServiceRules     int  `json:"service_rules"`
	WildcardServices bool `json:"wildcard_services"`
	AllowFirewall    bool `json:"allow_firewall"`
	AllowKill        bool `json:"allow_kill"`
	AllowUpdate      bool `json:"allow_update"`
}

// This deliberately exposes a small projection of configuration: never serialize
// Config, command output, socket paths, user names, or connection errors here.
type diagnostics struct {
	UID      int               `json:"uid"`
	Mode     string            `json:"mode"`
	ReadOnly bool              `json:"read_only"`
	Tools    map[string]bool   `json:"tools"`
	Systemd  bool              `json:"systemd"`
	Helper   helperDiagnostics `json:"helper"`
	Warnings []string          `json:"warnings"`
	Notes    []string          `json:"notes"`
}

func diagnosticsMode(cfg *config.Config, uid int) string {
	if cfg.ReadOnly {
		return "read-only"
	}
	if uid == 0 {
		return "root"
	}
	if cfg.Helper != nil {
		return "helper"
	}
	return "unprivileged"
}

func collectDiagnostics(ctx context.Context, cfg *config.Config) diagnostics {
	uid := os.Geteuid()
	result := diagnostics{UID: uid, Mode: diagnosticsMode(cfg, uid), ReadOnly: cfg.ReadOnly, Tools: map[string]bool{}, Warnings: []string{}, Notes: []string{}}
	for _, name := range []string{"systemctl", "journalctl", "ufw", "firewall-cmd"} {
		result.Tools[name] = diagnosticToolAvailable(name, []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"})
	}
	info, err := os.Stat("/run/systemd/system")
	result.Systemd = err == nil && info.IsDir() && result.Tools["systemctl"]
	if !result.Systemd {
		result.Warnings = append(result.Warnings, "未检测到 systemd 运行环境，系统服务功能可能不可用。")
	}
	if !result.Tools["journalctl"] {
		result.Warnings = append(result.Warnings, "未安装 journalctl，无法读取系统日志。")
	}
	if !result.Tools["ufw"] && !result.Tools["firewall-cmd"] {
		result.Warnings = append(result.Warnings, "未找到 UFW 或 firewalld，防火墙管理不可用。")
	}
	if h := cfg.Helper; h != nil {
		result.Helper = helperDiagnostics{Configured: true, ServiceRules: len(h.Services), AllowFirewall: h.AllowFirewall, AllowKill: h.AllowKill, AllowUpdate: h.AllowUpdate}
		_, result.Helper.WildcardServices = h.Services["*"]
		dialer := net.Dialer{Timeout: 250 * time.Millisecond}
		conn, err := dialer.DialContext(ctx, "unix", h.Socket)
		if err == nil {
			result.Helper.Reachable = true
			conn.Close()
		}
		if uid != 0 && !result.Helper.Reachable {
			result.Warnings = append(result.Warnings, "Helper 无法连接；请检查服务是否启动以及面板用户的 socket 访问权限。")
		}
		result.Notes = append(result.Notes, "Helper 授权显示面板当前配置；连接成功不代表身份校验或操作授权通过，最终以 Helper 进程实际配置为准。")
		if uid == 0 {
			result.Notes = append(result.Notes, "当前进程以 root 运行，管理操作直接执行，不使用 Helper 授权。")
		}
	}
	if cfg.ReadOnly {
		result.Notes = append(result.Notes, "已启用只读模式，所有修改操作均被禁止。")
	}
	if uid != 0 {
		result.Notes = append(result.Notes, "文件操作与日志读取使用面板进程自身的 Linux 权限；Helper 不提升文件访问权限。")
		if cfg.Helper == nil && !cfg.ReadOnly {
			result.Warnings = append(result.Warnings, "当前为普通用户且未配置 Helper，服务控制、防火墙和系统更新可能因权限不足失败。")
		}
	}
	result.Notes = append(result.Notes, "工具存在和 systemd 运行目录仅表示基本环境可用，不保证服务总线、日志权限或防火墙守护进程正常。")
	return result
}

// Match the fixed search path and executable-file checks used by both runners.
// The process PATH is intentionally ignored, including user-supplied tools.
func diagnosticToolAvailable(name string, dirs []string) bool {
	for _, dir := range dirs {
		info, err := os.Stat(filepath.Join(dir, name))
		if err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
			return true
		}
	}
	return false
}

func handleDiagnostics(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(collectDiagnostics(r.Context(), cfg))
	}
}
