package sysinfo

import (
	"context"
	"net/http"
	"strings"
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

const (
	appStepTimeout     = 6 * time.Minute
	appInstallDeadline = 9 * time.Minute // helper connection cap is 10 minutes
)

// AppManager serves the catalog and runs installs as task-center tasks:
// package managers can run for minutes, which would otherwise hit the
// server's write timeout.
type AppManager struct {
	SiteManager
}

func NewAppManager(tasks *TaskManager) *AppManager {
	return &AppManager{SiteManager: SiteManager{Run: RunCommand, RunTimeout: RunCommandTimeout, Tasks: tasks}}
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
// engine details reused from site detection. Database catalog entries take
// their state from the database engine probes.
func (m *AppManager) Apps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	manager := m.detectPackageManager(ctx)
	env := m.detectEnvironment(ctx)
	engines := make(map[string]EngineInfo, len(env))
	for _, e := range env {
		engines[e.Engine] = e
	}
	for _, e := range detectDatabaseEngines(ctx, m.Run) {
		if _, exists := engines[e.Engine]; !exists || !engines[e.Engine].Installed {
			engines[e.Engine] = e
		}
	}
	items := make([]AppInfo, 0, len(helper.AppCatalog))
	for _, spec := range helper.AppCatalog {
		info := AppInfo{Name: spec.Name, Title: spec.Title, Description: spec.Description, Package: spec.Packages[manager]}
		if e, ok := engines[spec.Name]; ok {
			info.Installed, info.Running, info.Version, info.Detail = e.Installed, e.Running, e.Version, e.Detail
		}
		items = append(items, info)
	}
	JSON(w, struct {
		PackageManager string    `json:"package_manager"`
		Items          []AppInfo `json:"items"`
	}{manager, items})
}

// AppInstall validates the app name and starts the install as a background
// task-center task. The response returns immediately; progress is watched
// in the task center and polled via /api/tasks/{id}.
func (m *AppManager) AppInstall(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if !helper.ValidAppName(name) {
		http.Error(w, "unknown app", 400)
		return
	}
	tasks := m.TaskCenter()
	if tasks.Running("app-install", name) {
		http.Error(w, "an install job is already running", 409)
		return
	}
	title := name
	for _, spec := range helper.AppCatalog {
		if spec.Name == name {
			title = spec.Title
			break
		}
	}
	task := tasks.Start("app-install", name, "安装 "+title, func(ctx context.Context, appendOut func(string)) error {
		ctx, cancel := context.WithTimeout(ctx, appInstallDeadline)
		defer cancel()
		return m.install(ctx, name, appendOut)
	})
	JSON(w, map[string]string{"message": "install started", "app": name, "task_id": task.ID})
}

// install runs the catalog steps either through the privileged helper
// (which re-detects the package manager and re-validates everything) or
// directly with the panel's own privileges (root mode). Output is appended
// as it becomes available: per step in direct mode, once at the end when
// the helper returns.
func (m *AppManager) install(ctx context.Context, name string, appendOut func(string)) error {
	if out, routed, err := privileged(ctx, helper.Request{Op: helper.OpApp, Action: "install", App: name}); routed {
		appendOut(helper.TrimOutput(out))
		return err
	}
	manager := m.detectPackageManager(ctx)
	if manager == "" {
		return ErrUnavailable
	}
	steps, err := helper.AppInstallSteps(manager, name)
	if err != nil {
		return err
	}
	for _, step := range steps {
		out, err := m.RunTimeout(ctx, appStepTimeout, step[0], step[1:]...)
		appendOut(helper.TrimOutput(out))
		if err != nil {
			return err
		}
	}
	return nil
}
