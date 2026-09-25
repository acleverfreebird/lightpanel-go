//go:build linux

package helper

import (
	"context"
	"strings"
	"time"
)

// Package downloads can wait on slow mirrors; each step gets its own budget
// and the whole exchange is bounded by appInstallDeadline on the connection.
const (
	appStepTimeout     = 6 * time.Minute
	appInstallDeadline = 10 * time.Minute
)

// detectPackageManager probes the supported package managers in fixed order
// and returns the first usable binary, or "" when none exists.
func detectPackageManager() string {
	for _, bin := range PackageManagerBinaries {
		if _, err := run(context.Background(), bin, "--version"); err == nil {
			return bin
		}
	}
	return ""
}

// appInstall runs the catalog-built steps for one app. Every argument comes
// from AppInstallSteps; the panel's request only contributes the app name.
func (s *server) appInstall(req *Request) Response {
	if !ValidAppName(req.App) {
		return Response{Error: "unknown app"}
	}
	manager := detectPackageManager()
	if manager == "" {
		return Response{Error: "no supported package manager found (apt-get, dnf, yum, zypper, apk)"}
	}
	steps, err := AppInstallSteps(manager, req.App)
	if err != nil {
		return Response{Error: err.Error()}
	}
	var all strings.Builder
	for _, step := range steps {
		out, err := runTimeout(context.Background(), appStepTimeout, step[0], step[1:]...)
		all.WriteString(out)
		if err != nil {
			return Response{Output: TrimOutput(all.String()), Error: err.Error()}
		}
	}
	return Response{OK: true, Output: TrimOutput(all.String())}
}
