package sysinfo

import (
	"context"
	"fmt"
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
	Group       string `json:"group,omitempty"`
	Installed   bool   `json:"installed"`
	Running     bool   `json:"running"`
	Version     string `json:"version,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Package     string `json:"package,omitempty"`
}

const (
	appStepTimeout  = 6 * time.Minute
	appTaskDeadline = 9 * time.Minute // helper connection cap is 10 minutes
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

// detectApps is the shared state probe behind the store listing: it detects
// the package manager, the web engines and the database engines, then merges
// them into one AppInfo per catalog entry. AppInstall reuses it so install
// gating and the listing always see the same installed state.
func (m *AppManager) detectApps(ctx context.Context) (string, []AppInfo) {
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
		info := AppInfo{Name: spec.Name, Title: spec.Title, Description: spec.Description, Group: spec.Group, Package: spec.Packages[manager]}
		if e, ok := engines[spec.Name]; ok {
			info.Installed, info.Running, info.Version, info.Detail = e.Installed, e.Running, e.Version, e.Detail
		}
		items = append(items, info)
	}
	return manager, items
}

// Apps reports the package manager and the catalog with detected state.
func (m *AppManager) Apps(w http.ResponseWriter, r *http.Request) {
	manager, items := m.detectApps(r.Context())
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
	if tasks.Running("app-install", name) || tasks.Running("app-remove", name) {
		http.Error(w, "an install or remove job is already running", 409)
		return
	}
	// Group exclusivity: at most one app per catalog group (the two web
	// servers share ports 80/443 and the site engine selection) may be
	// installed. Live detected state, not the store listing cache, decides.
	if blocker := helper.AppGroupConflict(name, m.installedApps(r.Context())); blocker != "" {
		http.Error(w, fmt.Sprintf("无法安装 %s：%s 已安装，同一类型的软件只能安装一个（可先卸载已安装的那个）。", appTitle(name), appTitle(blocker)), 409)
		return
	}
	title := appTitle(name)
	task := tasks.Start("app-install", name, "安装 "+title, func(ctx context.Context, appendOut func(string)) error {
		ctx, cancel := context.WithTimeout(ctx, appTaskDeadline)
		defer cancel()
		return m.install(ctx, name, appendOut)
	})
	JSON(w, map[string]string{"message": "install started", "app": name, "task_id": task.ID})
}

// AppRemove mirrors AppInstall for uninstalling: the removal also runs as a
// background task-center task. A remove and an install for the same app are
// mutually exclusive — running either blocks the other. apt purge also clears
// the leftovers of a half-configured failed install, which is the main way to
// recover from one.
func (m *AppManager) AppRemove(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if !helper.ValidAppName(name) {
		http.Error(w, "unknown app", 400)
		return
	}
	tasks := m.TaskCenter()
	if tasks.Running("app-install", name) || tasks.Running("app-remove", name) {
		http.Error(w, "an install or remove job is already running", 409)
		return
	}
	task := tasks.Start("app-remove", name, "卸载 "+appTitle(name), func(ctx context.Context, appendOut func(string)) error {
		ctx, cancel := context.WithTimeout(ctx, appTaskDeadline)
		defer cancel()
		return m.remove(ctx, name, appendOut)
	})
	JSON(w, map[string]string{"message": "remove started", "app": name, "task_id": task.ID})
}

// installedApps probes the live install state of every catalog app and
// returns it as a name set for the group-exclusivity check.
func (m *AppManager) installedApps(ctx context.Context) map[string]bool {
	_, items := m.detectApps(ctx)
	installed := make(map[string]bool, len(items))
	for _, item := range items {
		if item.Installed {
			installed[item.Name] = true
		}
	}
	return installed
}

// appTitle resolves a catalog app key to its display title.
func appTitle(name string) string {
	for _, spec := range helper.AppCatalog {
		if spec.Name == name {
			return spec.Title
		}
	}
	return name
}

// install runs the catalog steps either through the privileged helper
// (which re-detects the package manager and re-validates everything) or
// directly with the panel's own privileges (root mode). Output is appended
// as it becomes available: per step in direct mode, once at the end when
// the helper returns.
func (m *AppManager) install(ctx context.Context, name string, appendOut func(string)) error {
	return m.runAppSteps(ctx, helper.Request{Op: helper.OpApp, Action: "install", App: name}, helper.AppInstallSteps, appendOut)
}

// remove runs the catalog removal steps either through the privileged helper
// or directly with the panel's own privileges (root mode), mirroring install.
func (m *AppManager) remove(ctx context.Context, name string, appendOut func(string)) error {
	return m.runAppSteps(ctx, helper.Request{Op: helper.OpApp, Action: "remove", App: name}, helper.AppRemoveSteps, appendOut)
}

// runAppSteps executes the steps built by the given catalog function, either
// through the privileged helper (which re-detects the package manager and
// re-validates everything) or directly in root mode. Output is appended as it
// becomes available: per step in direct mode, once at the end when the helper
// returns. Optional steps are housekeeping — their failure is logged but does
// not abort the sequence; a required step's failure does.
func (m *AppManager) runAppSteps(ctx context.Context, req helper.Request, build func(manager, app string) ([]helper.Step, error), appendOut func(string)) error {
	if out, routed, err := privileged(ctx, req); routed {
		appendOut(helper.TrimOutput(out))
		return err
	}
	manager := m.detectPackageManager(ctx)
	if manager == "" {
		return ErrUnavailable
	}
	steps, err := build(manager, req.App)
	if err != nil {
		return err
	}
	for _, step := range steps {
		out, err := m.RunTimeout(ctx, appStepTimeout, step.Args[0], step.Args[1:]...)
		appendOut(helper.TrimOutput(out))
		if err != nil && !step.Optional {
			return err
		}
	}
	return nil
}
