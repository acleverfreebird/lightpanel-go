package sysinfo

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"lightpanel/pkg/helper"
)

// App store: list installable server software and run installs. The catalog,
// the package-manager whitelist and every install argv live in pkg/helper so
// the panel and the privileged helper accept exactly the same inputs; this
// file only detects state and executes (or forwards) the built steps.

// AppInfo is one catalog app with the locally detected state.
type AppInfo struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Installed   bool   `json:"installed"`
	Running     bool   `json:"running"`
	Version     string `json:"version,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Package     string `json:"package,omitempty"`
}

// AppJob reports the state of the background install. Installs run as a
// single-slot job instead of inside the HTTP request: package managers can
// run for minutes, which would otherwise hit the server's write timeout.
type AppJob struct {
	App    string `json:"app,omitempty"`
	State  string `json:"state,omitempty"` // "", "running", "done", "error"
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}

const (
	appStepTimeout     = 6 * time.Minute
	appInstallDeadline = 9 * time.Minute // helper connection cap is 10 minutes
)

type AppManager struct {
	SiteManager
	mu  sync.Mutex
	job AppJob
}

func NewAppManager() *AppManager {
	return &AppManager{SiteManager: SiteManager{Run: RunCommand, RunTimeout: RunCommandTimeout}}
}

func (m *AppManager) detectPackageManager(ctx context.Context) string {
	for _, bin := range helper.PackageManagerBinaries {
		if _, err := m.Run(ctx, bin, "--version"); err == nil {
			return bin
		}
	}
	return ""
}

// Apps reports the package manager, the catalog with detected state and the
// engine details reused from site detection.
func (m *AppManager) Apps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	manager := m.detectPackageManager(ctx)
	env := m.detectEnvironment(ctx)
	engines := make(map[string]EngineInfo, len(env))
	for _, e := range env {
		engines[e.Engine] = e
	}
	items := make([]AppInfo, 0, len(helper.AppCatalog))
	for _, spec := range helper.AppCatalog {
		info := AppInfo{Name: spec.Name, Title: spec.Title, Description: spec.Description, Package: spec.Packages[manager]}
		if e, ok := engines[spec.Name]; ok {
			info.Installed, info.Running, info.Version, info.Detail = e.Installed, e.Running, e.Version, e.Detail
		}
		items = append(items, info)
	}
	m.mu.Lock()
	job := m.job
	m.mu.Unlock()
	JSON(w, struct {
		PackageManager string    `json:"package_manager"`
		Items          []AppInfo `json:"items"`
		Job            AppJob    `json:"job"`
	}{manager, items, job})
}

// AppInstall validates the app name, reserves the single install slot and
// starts the install in the background. The response returns immediately;
// progress is polled via InstallJob.
func (m *AppManager) AppInstall(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if !helper.ValidAppName(name) {
		http.Error(w, "unknown app", 400)
		return
	}
	m.mu.Lock()
	if m.job.State == "running" {
		m.mu.Unlock()
		http.Error(w, "an install job is already running", 409)
		return
	}
	m.job = AppJob{App: name, State: "running"}
	m.mu.Unlock()
	go m.runInstall(name)
	JSON(w, map[string]string{"message": "install started", "app": name})
}

func (m *AppManager) runInstall(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), appInstallDeadline)
	defer cancel()
	out, err := m.install(ctx, name)
	job := AppJob{App: name, State: "done", Output: helper.TrimOutput(out)}
	if err != nil {
		job.State, job.Error = "error", err.Error()
	}
	m.mu.Lock()
	m.job = job
	m.mu.Unlock()
}

// install runs the catalog steps either through the privileged helper
// (which re-detects the package manager and re-validates everything) or
// directly with the panel's own privileges (root mode).
func (m *AppManager) install(ctx context.Context, name string) (string, error) {
	if out, routed, err := privileged(ctx, helper.Request{Op: helper.OpApp, Action: "install", App: name}); routed {
		return out, err
	}
	manager := m.detectPackageManager(ctx)
	if manager == "" {
		return "", ErrUnavailable
	}
	steps, err := helper.AppInstallSteps(manager, name)
	if err != nil {
		return "", err
	}
	var all strings.Builder
	for _, step := range steps {
		out, err := m.RunTimeout(ctx, appStepTimeout, step[0], step[1:]...)
		all.WriteString(out)
		if err != nil {
			return all.String(), err
		}
	}
	return all.String(), nil
}

// InstallJob reports the current (or last) install job for polling.
func (m *AppManager) InstallJob(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	job := m.job
	m.mu.Unlock()
	JSON(w, job)
}
