package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"golang.org/x/crypto/bcrypt"

	"lightpanel/pkg/helper"
)

type Config struct {
	Host         string `toml:"host"`
	Port         int    `toml:"port"`
	AdminUser    string `toml:"admin_user"`
	PasswordHash string `toml:"password_hash"`
	// Deprecated: 文件管理已改为管理整个文件系统；保留此字段仅为兼容旧配置文件。
	SandboxRoot  string `toml:"sandbox_root"`
	TLSCert      string `toml:"tls_cert"`
	TLSKey       string `toml:"tls_key"`
	PublicOrigin string `toml:"public_origin"`
	LogFile      string `toml:"log_file"`
	ReadOnly     bool   `toml:"read_only"`
	MaxUploadMB  int    `toml:"max_upload_mb"`
	UpdateRepo   string `toml:"update_repo"`
	UpdateMirror string `toml:"update_mirror"`

	// Helper 为空表示不使用最小特权 helper：面板保持旧有行为（root 下直接执行
	// 特权操作）。配置了 [helper] 且面板以非 root 用户运行时，systemd 服务控制、
	// 防火墙、进程信号、自更新与托管站点操作会转发给独立的 helper 进程并按白名单授权。
	Helper *HelperConfig `toml:"helper"`
}

// HelperConfig 同时服务于两个进程：面板（Socket 用于转发请求）和
// `lightpanel helper` 子命令（AllowedUsers/Services/Allow* 为授权边界）。
type HelperConfig struct {
	Socket string `toml:"socket"`
	// AllowedUsers 允许连接 helper socket 的用户；helper 以其中第一个用户的
	// 属主权限收紧 socket 文件权限，并在每次连接时用 SO_PEERCRED 复核 UID。
	AllowedUsers []string `toml:"allowed_users"`
	// Services 按单元细分 systemd 授权："单元名" = 允许的动作列表；
	// "*" 条目把动作授予所有单元。空表 = 拒绝所有服务控制。
	Services map[string][]string `toml:"services"`
	// AllowFirewall 允许读写 ufw/firewalld 端口规则（参数在 helper 端白名单重建）。
	AllowFirewall bool `toml:"allow_firewall"`
	// AllowKill 允许对任意进程发送 SIGTERM/SIGKILL（身份经 pidfd 固定）。
	AllowKill bool `toml:"allow_kill"`
	// AllowUpdate 允许安装经 SHA256 校验的在线更新并重启面板服务。
	AllowUpdate bool `toml:"allow_update"`
	// AllowSites 允许托管站点操作：写入站点配置（helper 端从校验后的参数
	// 重新生成内容）、删除带托管标记的配置、重载引擎与 certbot 证书签发。
	AllowSites bool `toml:"allow_sites"`
	// AllowApps 允许应用商店安装：helper 通过系统软件包管理器安装固定目录
	// 中的应用；应用名经白名单校验，全部参数在 helper 端重建。
	AllowApps bool `toml:"allow_apps"`
	// AllowDatabases 允许数据库管理操作：列出/创建/删除数据库与用户管理；
	// 引擎、名称与全部参数在 helper 端重建，密码仅经 stdin 传递。
	AllowDatabases bool `toml:"allow_databases"`
	// StagingDir 是非 root 面板下载更新资产的目录（属主必须是面板用户，权限
	// 不得对组/其他用户可写；helper 安装前会复核）。
	StagingDir string `toml:"staging_dir"`
}

func LoadConfig(path string) (*Config, error) {
	c := &Config{Host: "127.0.0.1", Port: 8888, AdminUser: "admin", UpdateRepo: "acleverfreebird/lightpanel-go"}
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if err := toml.NewDecoder(f).DisallowUnknownFields().Decode(c); err != nil {
			return nil, err
		}
	}
	for key, dst := range map[string]*string{"HOST": &c.Host, "ADMIN_USER": &c.AdminUser, "PASS_HASH": &c.PasswordHash, "TLS_CERT": &c.TLSCert, "TLS_KEY": &c.TLSKey, "PUBLIC_ORIGIN": &c.PublicOrigin, "LOG_FILE": &c.LogFile, "UPDATE_REPO": &c.UpdateRepo, "UPDATE_MIRROR": &c.UpdateMirror} {
		if v, ok := os.LookupEnv("LP_" + key); ok {
			*dst = v
		}
	}
	if v, ok := os.LookupEnv("LP_PORT"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("LP_PORT: %w", err)
		}
		c.Port = n
	}
	if v, ok := os.LookupEnv("LP_READ_ONLY"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, err
		}
		c.ReadOnly = b
	}
	if v, ok := os.LookupEnv("LP_HELPER_SOCKET"); ok && c.Helper != nil {
		c.Helper.Socket = v
	}
	if v, ok := os.LookupEnv("LP_MAX_UPLOAD_MB"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("LP_MAX_UPLOAD_MB: %w", err)
		}
		c.MaxUploadMB = n
	}
	if c.MaxUploadMB == 0 {
		c.MaxUploadMB = 32
	}
	if c.MaxUploadMB < 1 || c.MaxUploadMB > 2048 {
		return nil, fmt.Errorf("max_upload_mb must be 1..2048")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(c.UpdateRepo) {
		return nil, fmt.Errorf("update_repo must be owner/repo")
	}
	if c.UpdateMirror != "" {
		u, err := url.Parse(c.UpdateMirror)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || strings.HasSuffix(c.UpdateMirror, "/") {
			return nil, fmt.Errorf("update_mirror must be an http(s) origin without path or trailing slash")
		}
	}
	if net.ParseIP(c.Host) == nil || c.Port < 1 || c.Port > 65535 {
		return nil, fmt.Errorf("host must be an IP; port must be 1..65535")
	}
	if strings.TrimSpace(c.AdminUser) == "" {
		return nil, fmt.Errorf("admin_user required")
	}
	cost, err := bcrypt.Cost([]byte(c.PasswordHash))
	if err != nil || cost < 10 || cost > 14 {
		return nil, fmt.Errorf("password_hash must be bcrypt with cost 10..14; run -hash-password")
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("both tls_cert and tls_key required")
	}
	if c.TLSCert == "" && !net.ParseIP(c.Host).IsLoopback() {
		return nil, fmt.Errorf("without local TLS, host must be loopback; terminate HTTPS at a local reverse proxy")
	}
	if c.PublicOrigin == "" {
		scheme := "http"
		if c.TLSCert != "" {
			scheme = "https"
		}
		c.PublicOrigin = scheme + "://" + net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}
	u, err := url.Parse(c.PublicOrigin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("public_origin must be http(s)://host[:port], without trailing slash")
	}
	if u.Scheme == "http" && (!net.ParseIP(c.Host).IsLoopback() || !isLoopback(u.Hostname())) {
		return nil, fmt.Errorf("non-loopback access requires an HTTPS public_origin and TLS or a trusted reverse proxy")
	}
	if c.TLSCert != "" && u.Scheme != "https" {
		return nil, fmt.Errorf("TLS requires HTTPS public_origin")
	}
	if c.Helper != nil {
		if c.Helper.Socket == "" {
			c.Helper.Socket = "/run/lightpanel/helper.sock"
		}
		if c.Helper.StagingDir == "" {
			c.Helper.StagingDir = "/var/lib/lightpanel/update"
		}
		if err := validateHelper(c.Helper); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func validateHelper(h *HelperConfig) error {
	if !helper.ValidAbsPath(h.Socket) {
		return fmt.Errorf("helper.socket must be an absolute cleaned path without backslash or ..")
	}
	if !helper.ValidAbsPath(h.StagingDir) {
		return fmt.Errorf("helper.staging_dir must be an absolute cleaned path without backslash or ..")
	}
	if len(h.AllowedUsers) == 0 {
		return fmt.Errorf("helper.allowed_users must name at least one panel user")
	}
	for _, name := range h.AllowedUsers {
		if !helper.ValidUserName(name) {
			return fmt.Errorf("helper.allowed_users: invalid user name %q", name)
		}
	}
	return helper.ValidateServicesACL(h.Services)
}

func isLoopback(host string) bool { return host == "localhost" || net.ParseIP(host).IsLoopback() }
