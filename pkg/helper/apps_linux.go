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

// appInstall runs the catalog-built install steps for one app. Every argument
// comes from AppInstallSteps; the panel's request only contributes the app name.
func (s *server) appInstall(req *Request) Response {
	return s.runAppSteps(req, AppInstallSteps)
}

// appRemove runs the catalog-built removal steps for one app, mirroring
// appInstall with AppRemoveSteps (apt purge also clears a half-configured
// failed install).
func (s *server) appRemove(req *Request) Response {
	return s.runAppSteps(req, AppRemoveSteps)
}

// runAppSteps validates the app name, detects the package manager and runs
// the steps built by the given catalog function. The panel's request only
// contributes the app name and the install/remove choice. Optional steps are
// housekeeping: their failure is captured in the output but does not abort
// the sequence; a required step's failure does.
func (s *server) runAppSteps(req *Request, build func(manager, app string) ([]Step, error)) Response {
	if !ValidAppName(req.App) {
		return Response{Error: "unknown app"}
	}
	manager := detectPackageManager()
	if manager == "" {
		return Response{Error: "no supported package manager found (apt-get, dnf, yum, zypper, apk)"}
	}
	steps, err := build(manager, req.App)
	if err != nil {
		return Response{Error: err.Error()}
	}
	var all strings.Builder
	for _, step := range steps {
		out, err := runTimeout(context.Background(), appStepTimeout, step.Args[0], step.Args[1:]...)
		all.WriteString(out)
		if err != nil && !step.Optional {
			return Response{Output: TrimOutput(all.String()), Error: err.Error()}
		}
	}
	return Response{OK: true, Output: TrimOutput(all.String())}
}
