package sysinfo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"lightpanel/pkg/helper"
)

// Database management: detect installed database engines, list databases and
// users, and run create/drop, user, backup and console operations. The
// operation set, the engines and every argv live in pkg/helper (shared with
// the privileged helper, which rebuilds and re-validates everything); this
// file only detects state, executes (or forwards) the built commands and
// keeps the BT-style credential notes. Passwords travel in the helper Request
// and reach the client binary over stdin, never argv.

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

// Timeouts. Backup/restore run inside task-center jobs, so they may take the
// same window the helper grants; the console is interactive.
const (
	dbTaskTimeout  = 29 * time.Minute
	dbQueryTimeout = 60 * time.Second
)

const (
	taskKindDBBackup  = "database-backup"
	taskKindDBRestore = "database-restore"
)

// DatabaseManager reuses SiteManager's Runner plumbing (Run/RunTimeout and
// the engineRunning probe) and adds stdin/stdout-capable runners for password
// input and streamed dumps, plus the task center and the credential store.
type DatabaseManager struct {
	SiteManager
	RunStdin  func(context.Context, time.Duration, string, string, ...string) (string, error)
	RunStream func(context.Context, time.Duration, io.Reader, io.Writer, string, ...string) (string, error)
	Tasks     *TaskManager
	Store     *dbCredentialStore
}

func NewDatabaseManager(tasks *TaskManager) *DatabaseManager {
	return &DatabaseManager{
		SiteManager: SiteManager{Run: RunCommand, RunTimeout: RunCommandTimeout},
		RunStdin:    RunCommandStdin,
		RunStream:   RunCommandStream,
		Tasks:       tasks,
		Store:       newDBCredentialStore(),
	}
}

// dbEngineOrder is the display/detection order of the supported engines.
var dbEngineOrder = []string{helper.DBMysql, helper.DBMariadb, helper.DBPostgresql, helper.DBRedis}

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

// classifyMysqlBanner decides, from a `mysqld`/`mariadbd --version` banner,
// which engine the server belongs to. Both MySQL-family engines list `mysqld`
// as a candidate — Debian ships MariaDB's server under that alternative name
// and lightpanel's unprivileged-container fixup diverts it behind a wrapper —
// so the binary name alone would credit one server to both engines. The
// banner decides: MariaDB prints "… Ver <n>-MariaDB …" (the name also appears
// lowercased in its build tag), MySQL's banner never mentions it. ok is false
// when the output carries no version banner; diagnostics mysqld writes to
// stderr ahead of it (config files it cannot read) must not be taken for one.
func classifyMysqlBanner(out string) (engine string, version string, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, "Ver ")
		if i < 0 {
			continue
		}
		if rest := line[i+len("Ver "):]; rest == "" || rest[0] < '0' || rest[0] > '9' {
			continue
		}
		if strings.Contains(strings.ToLower(line), "mariadb") {
			return helper.DBMariadb, line, true
		}
		return helper.DBMysql, line, true
	}
	return "", "", false
}

// detectDatabaseEngines probes each engine's server binary for version, then
// systemd for the running state. Shared with the app store's catalog state.
func detectDatabaseEngines(ctx context.Context, run Runner) []EngineInfo {
	engines := make([]EngineInfo, 0, len(dbEngineOrder))
	for _, engine := range dbEngineOrder {
		info := EngineInfo{Engine: engine, Installed: false, Detail: ErrUnavailable.Error()}
		for _, bin := range helper.DBServerBinaries[engine] {
			out, err := run(ctx, bin, "--version")
			if err != nil {
				continue
			}
			version := firstLine(out)
			if engine == helper.DBMysql || engine == helper.DBMariadb {
				family, banner, ok := classifyMysqlBanner(out)
				if !ok || family != engine {
					continue
				}
				version = banner
			}
			info.Installed = true
			info.Version = version
			info.Detail = ""
			break
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

// dbRow is one databases-page row: the live listing from the engine plus the
// panel's stored credential note (user/password/host), when the panel itself
// provisioned the database.
type dbRow struct {
	helper.DBInfo
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
	Host     string `json:"host,omitempty"`
}

// listEngine returns the databases and users of one engine. With a helper
// configured the whole listing runs as root there; otherwise the panel must
// itself have the privileges the client binaries expect (root socket auth
// for MySQL/MariaDB, the postgres system user for PostgreSQL).
func (m *DatabaseManager) listEngine(ctx context.Context, engine string) (dbs []helper.DBInfo, users []string, err error) {
	if out, routed, err := privileged(ctx, helper.Request{Op: helper.OpDatabase, Action: "list", DB: engine}); routed {
		if err != nil {
			return nil, nil, err
		}
		var payload helper.DBListPayload
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
	return helper.ParseDBList(splitOutputLines(dbOut)), splitOutputLines(userOut), nil
}

// splitOutputLines trims and splits command output into non-empty lines.
func splitOutputLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// Databases reports engine state plus per-engine database/user listings. The
// database rows carry sizes, charsets and (when the panel provisioned them)
// the stored credentials. Listing failures (e.g. MySQL root without socket
// auth) are reported per engine in errors and never fail the whole response.
func (m *DatabaseManager) Databases(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	engines := m.detectDatabases(ctx)
	databases := make(map[string][]dbRow)
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
		rows := make([]dbRow, 0, len(dbs))
		for _, db := range dbs {
			drop := false
			for _, e := range systemDatabases[info.Engine] {
				if db.Name == e {
					drop = true
					break
				}
			}
			if drop {
				continue
			}
			row := dbRow{DBInfo: db}
			if m.Store != nil {
				if cred, ok := m.Store.get(dbCredentialKey("db", info.Engine, db.Name)); ok {
					row.User, row.Password, row.Host = cred.User, cred.Password, cred.Host
					if row.Charset == "" {
						row.Charset = cred.Charset
					}
				}
			}
			rows = append(rows, row)
		}
		databases[info.Engine] = rows
		users[info.Engine] = filterUserEntries(us, systemUsers[info.Engine])
	}
	// Unit candidates for the frontend's start/stop buttons (glob-free names).
	units := make(map[string][]string, len(engines))
	for _, info := range engines {
		units[info.Engine] = concreteUnits(helper.DBEngineUnits[info.Engine])
	}
	JSON(w, struct {
		Engines   []EngineInfo        `json:"engines"`
		Databases map[string][]dbRow  `json:"databases"`
		Users     map[string][]string `json:"users"`
		Units     map[string][]string `json:"units"`
		Errors    map[string]string   `json:"errors,omitempty"`
	}{engines, databases, users, units, listErrors})
}

// dbMutation validates one database/user operation form, forwards it to the
// helper when configured and otherwise executes the shared argv locally, then
// records the credential note on success.
func (m *DatabaseManager) dbMutation(w http.ResponseWriter, r *http.Request, action, message string) {
	engine := strings.TrimSpace(r.FormValue("engine"))
	name := strings.TrimSpace(r.FormValue("name"))
	password := r.FormValue("password")
	if !helper.ValidDBEngine(engine) {
		http.Error(w, "unsupported database engine", 400)
		return
	}
	var wantUser, wantPassword bool
	var user, charset, host string
	switch action {
	case "create-db", "drop-db":
		if action == "create-db" {
			// Optional one-step provisioning: an account (default suggestion
			// is the database name itself), charset and access host.
			user = strings.TrimSpace(r.FormValue("user"))
			charset = strings.TrimSpace(r.FormValue("charset"))
			host = strings.TrimSpace(r.FormValue("host"))
			if user != "" {
				if !helper.ValidDBUser(user) {
					http.Error(w, "invalid user name", 400)
					return
				}
				if !helper.ValidDBPassword(password) {
					http.Error(w, "invalid password", 400)
					return
				}
			}
			if charset != "" && !helper.ValidDBCharset(engine, charset) {
				http.Error(w, "unsupported character set", 400)
				return
			}
			if host != "" && !helper.ValidDBHost(host) {
				http.Error(w, "invalid access host", 400)
				return
			}
		}
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
	req := helper.Request{Op: helper.OpDatabase, Action: action, DB: engine, Name: name,
		Password: password, User: user, Charset: charset, Host: host}
	if out, routed, err := privileged(r.Context(), req); routed {
		if err != nil {
			commandError(w, out, err)
			return
		}
		m.noteMutation(engine, name, user, password, charset, host, action)
		JSON(w, map[string]string{"message": message})
		return
	}
	var bin string
	var args []string
	var stdin string
	var err error
	if action == "create-db" {
		bin, args, stdin, err = helper.DBCreateCommand(engine, name,
			helper.DBCreateParams{User: user, Password: password, Charset: charset, Host: host})
	} else {
		bin, args, stdin, err = helper.DBCommand(engine, action, name, password)
	}
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
	m.noteMutation(engine, name, user, password, charset, host, action)
	JSON(w, map[string]string{"message": message})
}

// noteMutation records the credential note after a successful mutation.
// Storage failures are logged and never fail the mutation itself: the
// database exists either way, only the convenience note is missing.
func (m *DatabaseManager) noteMutation(engine, name, user, password, charset, host, action string) {
	if m.Store == nil {
		return
	}
	var err error
	switch action {
	case "create-db":
		if user == "" {
			return
		}
		err = m.Store.upsert(dbCredentialKey("db", engine, name),
			dbCredential{User: user, Password: password, Host: host, Charset: charset})
	case "create-user":
		err = m.Store.upsert(dbCredentialKey("user", engine, name), dbCredential{Password: password})
	case "set-password":
		err = m.Store.rotatePassword(engine, name, password)
	case "drop-db":
		err = m.Store.delete(dbCredentialKey("db", engine, name))
	}
	if err != nil {
		slog.Warn("database_credential_note", "engine", engine, "name", name, "error", err)
	}
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

// dbTarget validates the engine/database pair shared by every backup and
// console route.
func dbTarget(w http.ResponseWriter, r *http.Request) (engine, name string, ok bool) {
	engine = strings.TrimSpace(r.FormValue("engine"))
	name = strings.TrimSpace(r.FormValue("name"))
	if !helper.ValidDBEngine(engine) || engine == helper.DBRedis {
		http.Error(w, "unsupported database engine", 400)
		return "", "", false
	}
	if !helper.ValidDBName(name) {
		http.Error(w, "invalid database name", 400)
		return "", "", false
	}
	return engine, name, true
}

// dbBackupEntry is one row of the backup listing (shared JSON shape with the
// helper's backup-list payload).
type dbBackupEntry struct {
	File     string `json:"file"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
}

// DBBackup starts a task that dumps one database into the managed backup
// directory; the task id is returned immediately.
func (m *DatabaseManager) DBBackup(w http.ResponseWriter, r *http.Request) {
	engine, name, ok := dbTarget(w, r)
	if !ok {
		return
	}
	if m.Tasks == nil {
		http.Error(w, "task center unavailable", 503)
		return
	}
	key := engine + "/" + name
	if m.Tasks.Running(taskKindDBBackup, key) {
		http.Error(w, "该数据库已有备份任务在执行，请等待完成", 409)
		return
	}
	task := m.Tasks.Start(taskKindDBBackup, key, "备份数据库 "+name, func(ctx context.Context, appendOut func(string)) error {
		if err := m.runBackup(ctx, engine, name); err != nil {
			return err
		}
		appendOut("备份完成，文件已存入 " + helper.DBBackupDir + "\n")
		return nil
	})
	JSON(w, map[string]string{"message": "备份任务已创建", "task_id": task.ID})
}

func (m *DatabaseManager) runBackup(ctx context.Context, engine, name string) error {
	if out, routed, err := privileged(ctx, helper.Request{Op: helper.OpDatabase, Action: "backup", DB: engine, Name: name}); routed {
		if err != nil {
			return fmt.Errorf("%v\n%s", err, helper.TrimOutput(out))
		}
		return nil
	}
	bin, args, err := helper.DBBackupCommand(engine, name)
	if err != nil {
		return err
	}
	cmd, err := helper.ResolveDBBinary(engine, bin, dbBinaryAvailable)
	if err != nil {
		return err
	}
	file, err := helper.BackupFileName(engine, name, time.Now())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(helper.DBBackupDir, 0o711); err != nil {
		return fmt.Errorf("创建备份目录失败：%v", err)
	}
	tmp, err := os.CreateTemp(helper.DBBackupDir, ".lightpanel-dbtmp-*")
	if err != nil {
		return fmt.Errorf("创建备份文件失败：%v", err)
	}
	tmpName := tmp.Name()
	defer func() { tmp.Close(); os.Remove(tmpName) }()
	gzw := helper.DumpGzipWriter(tmp)
	out, err := m.RunStream(ctx, dbTaskTimeout, nil, gzw, cmd, args...)
	if err != nil {
		return fmt.Errorf("导出失败：%v\n%s", err, out)
	}
	if err := gzw.Close(); err != nil {
		return fmt.Errorf("压缩失败：%v", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("写入备份文件失败：%v", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(helper.DBBackupDir, file)); err != nil {
		return err
	}
	return nil
}

// DBBackups lists the managed backups of one database, newest first.
func (m *DatabaseManager) DBBackups(w http.ResponseWriter, r *http.Request) {
	engine, name, ok := dbTarget(w, r)
	if !ok {
		return
	}
	if out, routed, err := privileged(r.Context(), helper.Request{Op: helper.OpDatabase, Action: "backup-list", DB: engine, Name: name}); routed {
		if err != nil {
			commandError(w, out, err)
			return
		}
		var list []dbBackupEntry
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		JSON(w, map[string]any{"backups": list})
		return
	}
	list, err := localBackupList(helper.DBBackupDir, engine, name)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	JSON(w, map[string]any{"backups": list})
}

func localBackupList(dir, engine, name string) ([]dbBackupEntry, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []dbBackupEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	list := make([]dbBackupEntry, 0)
	for _, entry := range entries {
		if !helper.ValidBackupFile(engine, name, entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		list = append(list, dbBackupEntry{File: entry.Name(), Size: info.Size(), Modified: info.ModTime().Unix()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Modified > list[j].Modified })
	return list, nil
}

// DBBackupRestore starts a task that replays one managed backup into its
// database, replacing the data the dump contains.
func (m *DatabaseManager) DBBackupRestore(w http.ResponseWriter, r *http.Request) {
	engine, name, ok := dbTarget(w, r)
	if !ok {
		return
	}
	file := strings.TrimSpace(r.FormValue("file"))
	if !helper.ValidBackupFile(engine, name, file) {
		http.Error(w, "unknown backup file", 400)
		return
	}
	if m.Tasks == nil {
		http.Error(w, "task center unavailable", 503)
		return
	}
	key := engine + "/" + name
	if m.Tasks.Running(taskKindDBRestore, key) || m.Tasks.Running(taskKindDBBackup, key) {
		http.Error(w, "该数据库已有备份/恢复任务在执行，请等待完成", 409)
		return
	}
	task := m.Tasks.Start(taskKindDBRestore, key, "恢复数据库 "+name, func(ctx context.Context, appendOut func(string)) error {
		if err := m.runRestore(ctx, engine, name, file); err != nil {
			return err
		}
		appendOut("恢复完成：" + file + " 已导入 " + name + "\n")
		return nil
	})
	JSON(w, map[string]string{"message": "恢复任务已创建", "task_id": task.ID})
}

func (m *DatabaseManager) runRestore(ctx context.Context, engine, name, file string) error {
	if out, routed, err := privileged(ctx, helper.Request{Op: helper.OpDatabase, Action: "backup-restore", DB: engine, Name: name, File: file}); routed {
		if err != nil {
			return fmt.Errorf("%v\n%s", err, helper.TrimOutput(out))
		}
		return nil
	}
	bin, args, err := helper.DBRestoreCommand(engine, name)
	if err != nil {
		return err
	}
	cmd, err := helper.ResolveDBBinary(engine, bin, dbBinaryAvailable)
	if err != nil {
		return err
	}
	path := filepath.Join(helper.DBBackupDir, file)
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return errors.New("备份文件不可用")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zr, err := helper.DumpGzipReader(f)
	if err != nil {
		return errors.New("备份文件不是有效的 gzip 流")
	}
	defer zr.Close()
	out, err := m.RunStream(ctx, dbTaskTimeout, zr, io.Discard, cmd, args...)
	if err != nil {
		return fmt.Errorf("导入失败：%v\n%s", err, out)
	}
	return nil
}

// DBBackupDelete removes one managed backup file.
func (m *DatabaseManager) DBBackupDelete(w http.ResponseWriter, r *http.Request) {
	engine, name, ok := dbTarget(w, r)
	if !ok {
		return
	}
	file := strings.TrimSpace(r.FormValue("file"))
	if !helper.ValidBackupFile(engine, name, file) {
		http.Error(w, "unknown backup file", 400)
		return
	}
	if out, routed, err := privileged(r.Context(), helper.Request{Op: helper.OpDatabase, Action: "backup-delete", DB: engine, Name: name, File: file}); routed {
		if err != nil {
			commandError(w, out, err)
			return
		}
		JSON(w, map[string]string{"message": "备份已删除"})
		return
	}
	path := filepath.Join(helper.DBBackupDir, file)
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, "备份文件不可用", 404)
		return
	}
	if err := os.Remove(path); err != nil {
		commandError(w, "", err)
		return
	}
	JSON(w, map[string]string{"message": "备份已删除"})
}

// DBBackupDownload streams one managed backup file to the browser. In helper
// mode the file was chowned to the panel user at creation precisely so this
// unprivileged process can read it; the validated name cannot leave the
// backup directory.
func (m *DatabaseManager) DBBackupDownload(w http.ResponseWriter, r *http.Request) {
	engine, name, ok := dbTarget(w, r)
	if !ok {
		return
	}
	file := strings.TrimSpace(r.FormValue("file"))
	if !helper.ValidBackupFile(engine, name, file) {
		http.Error(w, "unknown backup file", 404)
		return
	}
	path := filepath.Join(helper.DBBackupDir, file)
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, "备份文件不可用", 404)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "备份文件不可用", 404)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+file+`"`)
	http.ServeContent(w, r, file, st.ModTime(), f)
}

// DBQuery runs one SQL batch against one database (the built-in console,
// standing in for BT Panel's phpMyAdmin entry) and returns the raw client
// output: TSV with a header row.
func (m *DatabaseManager) DBQuery(w http.ResponseWriter, r *http.Request) {
	engine, name, ok := dbTarget(w, r)
	if !ok {
		return
	}
	sql := r.FormValue("sql")
	if !helper.ValidDBSQL(sql) {
		http.Error(w, "SQL 为空或超过 32 KiB", 400)
		return
	}
	if out, routed, err := privileged(r.Context(), helper.Request{Op: helper.OpDatabase, Action: "query", DB: engine, Name: name, SQL: sql}); routed {
		if err != nil {
			commandError(w, out, err)
			return
		}
		JSON(w, map[string]string{"output": out})
		return
	}
	bin, args, err := helper.DBQueryCommand(engine, name)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	cmd, err := helper.ResolveDBBinary(engine, bin, dbBinaryAvailable)
	if err != nil {
		commandError(w, "", ErrUnavailable)
		return
	}
	out, err := m.RunStdin(r.Context(), dbQueryTimeout, sql, cmd, args...)
	if err != nil {
		commandError(w, out, err)
		return
	}
	JSON(w, map[string]string{"output": out})
}
