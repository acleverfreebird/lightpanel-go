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
	return c, nil
}

func isLoopback(host string) bool { return host == "localhost" || net.ParseIP(host).IsLoopback() }
