package sysinfo

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"lightpanel/pkg/helper"
)

// Database management: detect installed database engines, list databases and
// users, and run create/drop and user operations. The operation set, the
// engines and every argv live in pkg/helper (shared with the privileged
// helper, which rebuilds and re-validates everything); this file only detects
// state and executes (or forwards) the built commands. Passwords travel in
// the helper Request and reach the client binary over stdin, never argv.

// SystemDatabases and SystemUsers are filtered out of listings; they exist on
// every install and would only add noise to the management page.
var systemDatabases = map[string][]string{
	helper.DBMysql:      {"information_schema", "mysql", "performance_schema", "sys"},
	helper.DBMariadb:    {"information_schema", "mysql", "performance_schema", "sys"},
	helper.DBPostgresql: nil,
	helper.DBRedis:      nil,
}

var systemUsers = map[string][]string{
	helper.DBMysql:      {"mysql.sys", "mysql.session", "mysql.infoschema", "mysql.infrastructure", "debian-sys-maint", "healthcheck"},
	helper.DBMariadb:    {"mysql.sys", "mysql.session", "mysql.infoschema", "debian-sys-maint", "mariadb.sys", "mariadb.session", "healthcheck"},
	helper.DBPostgresql: nil,
	helper.DBRedis:      nil,
}

// DatabaseManager reuses SiteManager's Runner plumbing (Run/RunTimeout and
// the engineRunning probe) and adds a stdin-capable runner for password input.
type DatabaseManager struct {
	SiteManager
	RunStdin func(context.Context, time.Duration, string, string, ...string) (string, error)
}

func NewDatabaseManager() *DatabaseManager {
	return &DatabaseManager{
		SiteManager: SiteManager{Run: RunCommand, RunTimeout: RunCommandTimeout},
		RunStdin:    RunCommandStdin,
	}
}

// dbEngineOrder is the display/detection order of the supported engines.
var dbEngineOrder = []string{helper.DBMysql, helper.DBMariadb, helper.DBPostgresql, helper.DBRedis}

// dbListPayload mirrors the JSON the helper's list action emits; the local
// (root) path builds the same shape.
type dbListPayload struct {
	Databases []string `json:"databases"`
	Users     []string `json:"users"`
}

// dbBinaryAvailable reports whether a whitelisted binary exists without
// running it — client candidates are substituted by existence, not by
// probing (running `mysql` bare would open an interactive session). A
// package variable so tests can stub the filesystem check.
var dbBinaryAvailable = func(name string) bool {
	_, err := executable(name)
	return err == nil
}

// dbEngineActive reports whether any concrete unit candidate is active.
func dbEngineActive(ctx context.Context, run Runner, units []string) bool {
	for _, unit := range units {
		out, err := run(ctx, "systemctl", "is-active", "--", unit)
		if err == nil && strings.TrimSpace(out) == "active" {
			return true
		}
	}
	return false
}

// concreteUnits strips glob candidates (postgresql@*.service), which
// `systemctl is-active` cannot probe.
func concreteUnits(units []string) []string {
	var out []string
	for _, unit := range units {
		if !strings.ContainsAny(unit, "*?[") {
			out = append(out, unit)
		}
	}
	return out
}

// detectDatabaseEngines probes each engine's server binary for version, then
// systemd for the running state. Shared with the app store's catalog state.
func detectDatabaseEngines(ctx context.Context, run Runner) []EngineInfo {
	engines := make([]EngineInfo, 0, len(dbEngineOrder))
	for _, engine := range dbEngineOrder {
		info := EngineInfo{Engine: engine, Installed: false, Detail: ErrUnavailable.Error()}
		for _, bin := range helper.DBServerBinaries[engine] {
			if out, err := run(ctx, bin, "--version"); err == nil {
				info.Installed = true
				info.Version = firstLine(out)
				info.Detail = ""
				break
			}
		}
		if info.Installed {
			info.Running = dbEngineActive(ctx, run, concreteUnits(helper.DBEngineUnits[engine]))
		}
		engines = append(engines, info)
	}
	return engines
}

// detectDatabases probes engine state for the databases page.
func (m *DatabaseManager) detectDatabases(ctx context.Context) []EngineInfo {
	return detectDatabaseEngines(ctx, m.Run)
}

func filterNames(names []string, exclude []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		drop := false
		for _, e := range exclude {
			if name == e {
				drop = true
				break
			}
		}
		if !drop && name != "" {
			out = append(out, name)
		}
	}
	return out
}

// filterUserEntries filters user listing rows. MySQL rows carry "user\thost",
// so matching happens on the leading name field while the original row text
// (including the host) is preserved for display.
func filterUserEntries(entries []string, exclude []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		fields := strings.Fields(entry)
		if len(fields) == 0 {
			continue
		}
		drop := false
		for _, e := range exclude {
			if fields[0] == e {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}

// listEngine returns the databases and users of one engine. With a helper
// configured the whole listing runs as root there; otherwise the panel must
// itself have the privileges the client binaries expect (root socket auth
// for MySQL/MariaDB, the postgres system user for PostgreSQL).
func (m *DatabaseManager) listEngine(ctx context.Context, engine string) (dbs, users []string, err error) {
	if out, routed, err := privileged(ctx, helper.Request{Op: helper.OpDatabase, Action: "list", DB: engine}); routed {
		if err != nil {
			return nil, nil, err
		}
		var payload dbListPayload
		if err := json.Unmarshal([]byte(out), &payload); err != nil {
			return nil, nil, err
		}
		return payload.Databases, payload.Users, nil
	}
	userBin, userArgs, err := helper.DBUserCommand(engine)
	if err != nil {
		return nil, nil, err
	}
	userCmd, err := helper.ResolveDBBinary(engine, userBin, dbBinaryAvailable)
	if err != nil {
		return nil, nil, err
	}
	userOut, err := m.RunStdin(ctx, 8*time.Second, "", userCmd, userArgs...)
	if err != nil {
		return nil, nil, err
	}
	dbBin, dbArgs, _, err := helper.DBCommand(engine, "list", "", "")
	if err != nil {
		return nil, nil, err
	}
	dbCmd, err := helper.ResolveDBBinary(engine, dbBin, dbBinaryAvailable)
	if err != nil {
		return nil, nil, err
	}
	dbOut, err := m.RunStdin(ctx, 8*time.Second, "", dbCmd, dbArgs...)
	if err != nil {
		return nil, nil, err
	}
	return splitOutputLines(dbOut), splitOutputLines(userOut), nil
}

// splitOutputLines trims and splits command output into non-empty lines.
func splitOutputLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// Databases reports engine state plus per-engine database/user listings.
// Listing failures (e.g. MySQL root without socket auth) are reported per
// engine in errors and never fail the whole response.
func (m *DatabaseManager) Databases(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	engines := m.detectDatabases(ctx)
	databases := make(map[string][]string)
	users := make(map[string][]string)
	listErrors := make(map[string]string)
	for _, info := range engines {
		if !info.Installed || info.Engine == helper.DBRedis {
			continue
		}
		dbs, us, err := m.listEngine(ctx, info.Engine)
		if err != nil {
			listErrors[info.Engine] = helper.TrimOutput(err.Error())
			continue
		}
		databases[info.Engine] = filterNames(dbs, systemDatabases[info.Engine])
		users[info.Engine] = filterUserEntries(us, systemUsers[info.Engine])
	}
	// Unit candidates for the frontend's start/stop buttons (glob-free names).
	units := make(map[string][]string, len(engines))
	for _, info := range engines {
		units[info.Engine] = concreteUnits(helper.DBEngineUnits[info.Engine])
	}
	JSON(w, struct {
		Engines   []EngineInfo        `json:"engines"`
		Databases map[string][]string `json:"databases"`
		Users     map[string][]string `json:"users"`
		Units     map[string][]string `json:"units"`
		Errors    map[string]string   `json:"errors,omitempty"`
	}{engines, databases, users, units, listErrors})
}

// dbMutation validates one database/user operation form, forwards it to the
// helper when configured and otherwise executes the shared argv locally.
func (m *DatabaseManager) dbMutation(w http.ResponseWriter, r *http.Request, action, message string) {
	engine := strings.TrimSpace(r.FormValue("engine"))
	name := strings.TrimSpace(r.FormValue("name"))
	password := r.FormValue("password")
	if !helper.ValidDBEngine(engine) {
		http.Error(w, "unsupported database engine", 400)
		return
	}
	var wantUser, wantPassword bool
	switch action {
	case "create-db", "drop-db":
	case "create-user", "set-password":
		wantUser, wantPassword = true, true
	default:
		http.Error(w, "unsupported database action", 400)
		return
	}
	if engine == helper.DBRedis {
		http.Error(w, "redis does not support database or user management", 400)
		return
	}
	if wantUser {
		if !helper.ValidDBUser(name) {
			http.Error(w, "invalid user name", 400)
			return
		}
		if wantPassword && !helper.ValidDBPassword(password) {
			http.Error(w, "invalid password", 400)
			return
		}
	} else if !helper.ValidDBName(name) {
		http.Error(w, "invalid database name", 400)
		return
	}
	req := helper.Request{Op: helper.OpDatabase, Action: action, DB: engine, Name: name, Password: password}
	if out, routed, err := privileged(r.Context(), req); routed {
		if err != nil {
			commandError(w, out, err)
			return
		}
		JSON(w, map[string]string{"message": message})
		return
	}
	bin, args, stdin, err := helper.DBCommand(engine, action, name, password)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	cmd, err := helper.ResolveDBBinary(engine, bin, dbBinaryAvailable)
	if err != nil {
		commandError(w, "", ErrUnavailable)
		return
	}
	out, err := m.RunStdin(r.Context(), 15*time.Second, stdin, cmd, args...)
	if err != nil {
		commandError(w, out, err)
		return
	}
	JSON(w, map[string]string{"message": message})
}

func (m *DatabaseManager) DBCreate(w http.ResponseWriter, r *http.Request) {
	m.dbMutation(w, r, "create-db", "数据库创建完成")
}

func (m *DatabaseManager) DBDrop(w http.ResponseWriter, r *http.Request) {
	m.dbMutation(w, r, "drop-db", "数据库已删除")
}

func (m *DatabaseManager) DBUserCreate(w http.ResponseWriter, r *http.Request) {
	m.dbMutation(w, r, "create-user", "用户创建完成")
}

func (m *DatabaseManager) DBUserPassword(w http.ResponseWriter, r *http.Request) {
	m.dbMutation(w, r, "set-password", "密码已更新")
}
