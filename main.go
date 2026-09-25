//go:build linux

package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
	"lightpanel/config"
	"lightpanel/pkg/sysinfo"
)

// Injected at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}
func run() error {
	configPath := flag.String("c", "", "TOML config (empty: environment/defaults)")
	hashPassword := flag.Bool("hash-password", false, "read password from terminal and print bcrypt hash")
	flag.Parse()
	if *hashPassword {
		return passwordHash()
	}
	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	sysinfo.BuildVersion = version
	sysinfo.UpdateRepo = cfg.UpdateRepo
	sysinfo.UpdateMirror = cfg.UpdateMirror
	logOutput := os.Stdout
	if cfg.LogFile != "" {
		logOutput, err = os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer logOutput.Close()
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(logOutput, nil)))
	// Soft Go memory target, not an RSS guarantee. Respect explicit operator tuning.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(40 << 20)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(75)
	}
	files, err := sysinfo.NewFiles(int64(cfg.MaxUploadMB) << 20)
	if err != nil {
		return fmt.Errorf("open filesystem root: %w", err)
	}
	defer files.Close()
	handler, err := newHandler(cfg, files, sysinfo.NewManager())
	if err != nil {
		return err
	}
	server := &http.Server{Addr: net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" {
			done <- server.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			done <- server.ListenAndServe()
		}
	}()
	slog.Info("starting", "address", server.Addr, "origin", cfg.PublicOrigin, "read_only", cfg.ReadOnly)
	select {
	case err := <-done:
		if err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		_ = server.Close()
		return err
	}
	slog.Info("stopped")
	return nil
}
func passwordHash() error {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return fmt.Errorf("run -hash-password in an interactive terminal (password is never accepted as a command argument)")
	}
	fmt.Fprint(os.Stderr, "Password (12..72 bytes): ")
	p, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	defer clear(p)
	if len(p) < 12 || len(p) > 72 {
		return fmt.Errorf("password must be 12..72 bytes")
	}
	fmt.Fprint(os.Stderr, "Confirm password: ")
	again, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	defer clear(again)
	if string(p) != string(again) {
		return fmt.Errorf("passwords do not match")
	}
	hash, err := bcrypt.GenerateFromPassword(p, 12)
	if err != nil {
		return err
	}
	fmt.Println(string(hash))
	return nil
}
