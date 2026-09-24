//go:build linux

package main

import (
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"lightpanel/config"
	"lightpanel/pkg/auth"
	"lightpanel/pkg/sysinfo"
)

//go:embed templates/*.html static/*
var embeddedFiles embed.FS

func newHandler(cfg *config.Config, files *sysinfo.Files, manager *sysinfo.Manager) (http.Handler, error) {
	tmpl, err := template.ParseFS(embeddedFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(embeddedFiles, "static")
	if err != nil {
		return nil, err
	}
	a := auth.New(cfg)
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /login", a.Login(tmpl))
	mux.HandleFunc("POST /login", a.Login(tmpl))
	register := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, a.Require(audit(cfg.AdminUser, h))) }
	register("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, "index.html", map[string]any{"User": cfg.AdminUser, "CSRF": auth.CSRF(r), "ReadOnly": cfg.ReadOnly, "UploadMB": files.UploadLimit() >> 20}); err != nil {
			slog.Error("render", "error", err)
		}
	})
	register("POST /logout", a.Logout)
	metrics := &sysinfo.MetricsReader{}
	register("GET /api/metrics", metrics.ServeHTTP)
	register("GET /api/processes", sysinfo.HandleProcesses)
	register("POST /api/process/kill", sysinfo.HandleProcessKill)
	register("GET /api/services", manager.Services)
	register("POST /api/service/action", manager.ServiceAction)
	register("GET /api/files", files.List)
	register("GET /api/file/download", files.Download)
	register("POST /api/file/upload", files.Upload)
	register("GET /api/file/read", files.Read)
	register("POST /api/file/write", files.Write)
	register("POST /api/file/mkdir", files.Mkdir)
	register("POST /api/file/rename", files.Rename)
	register("POST /api/file/delete", files.Delete)
	register("POST /api/file/chmod", files.Chmod)
	register("GET /api/logs", manager.Logs)
	register("GET /api/firewall", manager.Firewall)
	register("POST /api/firewall/rule", manager.FirewallAction)
	return security(cfg, files.UploadLimit(), mux), nil
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func audit(user string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Metrics and process polling do not flood logs. File/log reads are sensitive.
		sensitive := r.Method == "POST" || strings.HasPrefix(r.URL.Path, "/api/file") || r.URL.Path == "/api/logs"
		sw := &statusWriter{ResponseWriter: w}
		if sensitive {
			slog.Info("audit_begin", "user", user, "ip", r.RemoteAddr, "method", r.Method, "route", r.URL.Path)
		}
		defer func() {
			if sensitive {
				status := sw.status
				if status == 0 {
					status = 200
				}
				target := r.URL.Query().Get("path")
				if target == "" {
					target = r.PostForm.Get("path")
				}
				slog.Info("audit_end", "user", user, "ip", r.RemoteAddr,
					"route", r.URL.Path, "status", status, "path", target,
					"unit", r.Form.Get("name"), "action", r.Form.Get("action"),
					"to", r.Form.Get("to"), "recursive", r.Form.Get("recursive"),
					"pid", r.Form.Get("pid"), "signal", r.Form.Get("signal"),
					"engine", r.Form.Get("engine"), "port", r.Form.Get("port"),
					"protocol", r.Form.Get("protocol"), "mode", r.Form.Get("mode"))
			}
		}()
		next.ServeHTTP(sw, r)
	})
}
func security(cfg *config.Config, uploadLimit int64, next http.Handler) http.Handler {
	slots := make(chan struct{}, 8)
	origin, _ := url.Parse(cfg.PublicOrigin)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				slog.Error("panic", "route", r.URL.Path)
				http.Error(sw, "internal error", 500)
			}
			if sw.status >= 400 {
				slog.Warn("request_rejected", "ip", r.RemoteAddr, "route", r.URL.Path, "status", sw.status, "duration_ms", time.Since(start).Milliseconds())
			}
		}()
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Cache-Control", "no-store")
		if origin.Scheme == "https" {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if r.Host != origin.Host {
			http.Error(sw, "unrecognized host", 403)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(sw, "server busy", 503)
			return
		}
		limit := int64(16 << 10)
		switch r.URL.Path {
		case "/api/file/upload":
			limit = uploadLimit
		case "/api/file/write":
			limit = sysinfo.MaxEdit
		}
		r.Body = http.MaxBytesReader(sw, r.Body, limit)
		next.ServeHTTP(sw, r)
	})
}
