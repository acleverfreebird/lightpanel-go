package sysinfo

import (
	"context"
	"io"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"lightpanel/pkg/helper"
)

func dbTestManager(run Runner, runStdin func(context.Context, time.Duration, string, string, ...string) (string, error)) *DatabaseManager {
	if runStdin == nil {
		runStdin = func(ctx context.Context, _ time.Duration, stdin, name string, args ...string) (string, error) {
			return run(ctx, name, args...)
		}
	}
	timeout := func(ctx context.Context, _ time.Duration, name string, args ...string) (string, error) {
		return run(ctx, name, args...)
	}
	return &DatabaseManager{
		SiteManager: SiteManager{Run: run, RunTimeout: timeout},
		RunStdin:    runStdin,
		RunStream: func(ctx context.Context, _ time.Duration, stdin io.Reader, _ io.Writer, name string, args ...string) (string, error) {
			return run(ctx, name, args...)
		},
	}
}

func dbTestStore(t *testing.T) *dbCredentialStore {
	t.Helper()
	return &dbCredentialStore{path: filepath.Join(t.TempDir(), "credentials.json")}
}

func TestDatabasesHandlerDetectsNothingWithoutTools(t *testing.T) {
	m := dbTestManager(func(context.Context, string, ...string) (string, error) { return "", ErrUnavailable }, nil)
	rec := httptest.NewRecorder()
	m.Databases(rec, httptest.NewRequest("GET", "/api/databases", nil))
	if rec.Code != 200 {
		t.Fatalf("Databases status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, engine := range []string{"mysql", "mariadb", "postgresql", "redis"} {
		if !strings.Contains(body, `"engine":"`+engine+`"`) {
			t.Errorf("response missing engine %q, got: %s", engine, body)
		}
	}
	if strings.Count(body, `"installed":true`) != 0 {
		t.Errorf("no engine should be detected when every probe fails: %s", body)
	}
}

func TestDatabasesHandlerListsAndFilters(t *testing.T) {
	// Tests run on machines without a real mysql client; stub the existence
	// check so the fake runner's output flows through the normal path.
	available := dbBinaryAvailable
	dbBinaryAvailable = func(name string) bool { return name == "mysql" }
	t.Cleanup(func() { dbBinaryAvailable = available })
	mysqlVersionProbes := 0
	m := dbTestManager(func(ctx context.Context, name string, args ...string) (string, error) {
		switch {
		case name == "mysqld" && len(args) == 1 && args[0] == "--version":
			// Answer only the mysql engine's probe; mariadb (which falls back
			// to mysqld) must stay undetected in this fixture.
			mysqlVersionProbes++
			if mysqlVersionProbes == 1 {
				return "mysqld  Ver 8.0.36", nil
			}
			return "", ErrUnavailable
		case name == "systemctl" && args[0] == "is-active":
			return "active", nil
		}
		return "", ErrUnavailable
	}, func(ctx context.Context, _ time.Duration, stdin, name string, args ...string) (string, error) {
		if name == "mysql" {
			for _, arg := range args {
				if strings.Contains(arg, "information_schema.SCHEMATA") {
					return "information_schema\tutf8mb4\t0\nshop\tutf8mb4\t20480\nmysql\tutf8mb4\t1024\n", nil
				}
				if strings.Contains(arg, "FROM mysql.user") {
					return "root\tlocalhost\napp_user\tlocalhost\nmysql.sys\tlocalhost\n", nil
				}
			}
		}
		return "", ErrUnavailable
	})
	rec := httptest.NewRecorder()
	m.Databases(rec, httptest.NewRequest("GET", "/api/databases", nil))
	if rec.Code != 200 {
		t.Fatalf("Databases status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"version":"mysqld  Ver 8.0.36"`) {
		t.Errorf("mysql version not reported: %s", body)
	}
	for _, want := range []string{`"name":"shop"`, `"charset":"utf8mb4"`, `"size":20480`} {
		if !strings.Contains(body, want) {
			t.Errorf("structured database row missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"name":"information_schema"`) || strings.Contains(body, `"name":"performance_schema"`) {
		t.Errorf("system databases must be filtered: %s", body)
	}
	if !strings.Contains(body, `app_user`) || strings.Contains(body, `mysql.sys`) {
		t.Errorf("user listing not filtered correctly: %s", body)
	}
}

func TestDatabasesHandlerShowsStoredCredentials(t *testing.T) {
	available := dbBinaryAvailable
	dbBinaryAvailable = func(name string) bool { return name == "mysql" }
	t.Cleanup(func() { dbBinaryAvailable = available })
	m := dbTestManager(func(ctx context.Context, name string, args ...string) (string, error) {
		switch {
		case name == "mysqld" && args[0] == "--version":
			return "mysqld  Ver 8.0.36", nil
		case name == "systemctl":
			return "active", nil
		}
		return "", ErrUnavailable
	}, func(ctx context.Context, _ time.Duration, stdin, name string, args ...string) (string, error) {
		if name == "mysql" {
			for _, arg := range args {
				if strings.Contains(arg, "information_schema.SCHEMATA") {
					return "shop\tutf8mb4\t20480\n", nil
				}
				if strings.Contains(arg, "FROM mysql.user") {
					return "shop\tlocalhost\n", nil
				}
			}
		}
		return "", ErrUnavailable
	})
	m.Store = dbTestStore(t)
	if err := m.Store.upsert(dbCredentialKey("db", "mysql", "shop"),
		dbCredential{User: "shop", Password: "s3cret", Host: "localhost"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec := httptest.NewRecorder()
	m.Databases(rec, httptest.NewRequest("GET", "/api/databases", nil))
	body := rec.Body.String()
	for _, want := range []string{`"user":"shop"`, `"password":"s3cret"`, `"host":"localhost"`} {
		if !strings.Contains(body, want) {
			t.Errorf("credential note missing %s: %s", want, body)
		}
	}
}

func TestDatabaseDetectionSplitsMysqlAndMariadb(t *testing.T) {
	// Both MySQL-family engines fall back to the shared `mysqld` name, so a
	// single server must light up exactly one of them — decided by the
	// version banner, not by which binary happened to answer. The wrapper
	// case also asserts diagnostics ahead of the banner are never reported
	// as the version.
	cases := []struct {
		name   string
		banner string
		engine string
	}{
		{"mysql", "mysqld  Ver 8.0.42 for Linux on x86_64 ((Ubuntu))", helper.DBMysql},
		{"mysql behind a noisy wrapper",
			"mysqld.distrib: File '/etc/mysql/mysql.conf.d/zz-lightpanel.cnf' not found (OS errno 13 - Permission denied)\n" +
				"mysqld  Ver 8.0.42 for Linux on x86_64 ((Ubuntu))", helper.DBMysql},
		{"mariadb under the mysqld name",
			"/usr/sbin/mysqld  Ver 10.11.6-MariaDB-0ubuntu0.24.04.1 for debian-linux-gnu on x86_64 (mariadb-1:10.11.6+maria~ubu2404)",
			helper.DBMariadb},
		{"mariadb native binary",
			"/usr/sbin/mariadbd  Ver 11.4.3-MariaDB for debian-linux-gnu on x86_64", helper.DBMariadb},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := dbTestManager(func(ctx context.Context, name string, args ...string) (string, error) {
				if len(args) == 1 && args[0] == "--version" && (name == "mysqld" || name == "mariadbd") {
					return c.banner, nil
				}
				return "", ErrUnavailable
			}, nil)
			byEngine := map[string]EngineInfo{}
			for _, e := range m.detectDatabases(context.Background()) {
				byEngine[e.Engine] = e
			}
			for _, engine := range []string{helper.DBMysql, helper.DBMariadb} {
				if got := byEngine[engine].Installed; got != (engine == c.engine) {
					t.Errorf("engine %s installed = %v, want %v (banner %q)", engine, got, engine == c.engine, c.banner)
				}
			}
			if v := byEngine[c.engine].Version; !strings.Contains(v, "Ver ") {
				t.Errorf("reported version %q must be the banner line, not a diagnostic", v)
			}
		})
	}
}

func TestDatabaseMutationsValidateInput(t *testing.T) {
	m := dbTestManager(func(context.Context, string, ...string) (string, error) { return "", ErrUnavailable }, nil)
	all := []string{"/api/databases/create", "/api/databases/delete", "/api/databases/user", "/api/databases/user-password"}
	cases := []struct {
		name  string
		form  url.Values
		paths []string
	}{
		{"unknown engine", url.Values{"engine": {"mongo"}, "name": {"app"}}, all},
		{"redis has no databases", url.Values{"engine": {"redis"}, "name": {"app"}}, all},
		{"bad database name", url.Values{"engine": {"mysql"}, "name": {"bad-name"}}, all},
		{"empty name", url.Values{"engine": {"mysql"}, "name": {""}}, all},
		{"bad user name", url.Values{"engine": {"mysql"}, "name": {"bad name"}, "password": {"secret"}}, all},
		{"empty password", url.Values{"engine": {"mysql"}, "name": {"app_user"}, "password": {""}},
			[]string{"/api/databases/user", "/api/databases/user-password"}},
		{"control character in password", url.Values{"engine": {"mysql"}, "name": {"app_user"}, "password": {"sec\nret"}},
			[]string{"/api/databases/user", "/api/databases/user-password"}},
		{"bad charset on create", url.Values{"engine": {"mysql"}, "name": {"app"}, "charset": {"utf8mb4'"}},
			[]string{"/api/databases/create"}},
		{"charset unsupported on postgresql", url.Values{"engine": {"postgresql"}, "name": {"app"}, "charset": {"utf8mb4"}},
			[]string{"/api/databases/create"}},
		{"bad access host", url.Values{"engine": {"mysql"}, "name": {"app"}, "host": {"evil'host"}},
			[]string{"/api/databases/create"}},
		{"user without password on create", url.Values{"engine": {"mysql"}, "name": {"app"}, "user": {"app_user"}},
			[]string{"/api/databases/create"}},
	}
	for _, c := range cases {
		for _, path := range c.paths {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("POST", path, strings.NewReader(c.form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			switch path {
			case "/api/databases/create":
				m.DBCreate(rec, req)
			case "/api/databases/delete":
				m.DBDrop(rec, req)
			case "/api/databases/user":
				m.DBUserCreate(rec, req)
			default:
				m.DBUserPassword(rec, req)
			}
			if rec.Code != 400 {
				t.Errorf("%s (%s): status = %d, want 400", c.name, path, rec.Code)
			}
		}
	}
}

func TestDatabaseCreateOneStepRunsBatchAndStoresNote(t *testing.T) {
	available := dbBinaryAvailable
	dbBinaryAvailable = func(name string) bool { return name == "mysql" }
	t.Cleanup(func() { dbBinaryAvailable = available })
	var gotStdin string
	m := dbTestManager(func(ctx context.Context, name string, args ...string) (string, error) {
		return "", nil
	}, func(ctx context.Context, _ time.Duration, stdin, name string, args ...string) (string, error) {
		gotStdin = stdin
		return "", nil
	})
	m.Store = dbTestStore(t)
	form := url.Values{"engine": {"mysql"}, "name": {"shop"}, "user": {"shop_user"},
		"password": {"s3cret"}, "charset": {"utf8mb4"}, "host": {"10.0.%.%"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/databases/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	m.DBCreate(rec, req)
	if rec.Code != 200 {
		t.Fatalf("DBCreate status = %d, body %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"CREATE DATABASE `shop` CHARACTER SET utf8mb4;",
		"CREATE USER 'shop_user'@'10.0.%.%' IDENTIFIED BY 's3cret';",
		"GRANT ALL PRIVILEGES ON `shop`.* TO 'shop_user'@'10.0.%.%';"} {
		if !strings.Contains(gotStdin, want) {
			t.Errorf("batch missing %q:\n%s", want, gotStdin)
		}
	}
	cred, ok := m.Store.get(dbCredentialKey("db", "mysql", "shop"))
	if !ok || cred.User != "shop_user" || cred.Password != "s3cret" || cred.Host != "10.0.%.%" {
		t.Errorf("credential note = %+v, ok %v", cred, ok)
	}
	// 删除数据库后备注一并清除。
	rec = httptest.NewRecorder()
	del := url.Values{"engine": {"mysql"}, "name": {"shop"}}
	req = httptest.NewRequest("POST", "/api/databases/delete", strings.NewReader(del.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	m.DBDrop(rec, req)
	if rec.Code != 200 {
		t.Fatalf("DBDrop status = %d", rec.Code)
	}
	if _, ok := m.Store.get(dbCredentialKey("db", "mysql", "shop")); ok {
		t.Errorf("credential note survived the drop")
	}
}

func TestDatabaseUserPasswordRotatesNotes(t *testing.T) {
	available := dbBinaryAvailable
	dbBinaryAvailable = func(name string) bool { return name == "mysql" }
	t.Cleanup(func() { dbBinaryAvailable = available })
	m := dbTestManager(func(context.Context, string, ...string) (string, error) { return "", nil }, nil)
	m.Store = dbTestStore(t)
	if err := m.Store.upsert(dbCredentialKey("db", "mysql", "shop"),
		dbCredential{User: "shop", Password: "old"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	form := url.Values{"engine": {"mysql"}, "name": {"shop"}, "password": {"new-pass"}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/databases/user-password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	m.DBUserPassword(rec, req)
	if rec.Code != 200 {
		t.Fatalf("DBUserPassword status = %d, body %s", rec.Code, rec.Body.String())
	}
	cred, _ := m.Store.get(dbCredentialKey("db", "mysql", "shop"))
	if cred.Password != "new-pass" {
		t.Errorf("db note password = %q, want rotated value", cred.Password)
	}
	userCred, ok := m.Store.get(dbCredentialKey("user", "mysql", "shop"))
	if !ok || userCred.Password != "new-pass" {
		t.Errorf("user note = %+v, ok %v", userCred, ok)
	}
}

func TestDatabaseBackupAndQueryValidateInput(t *testing.T) {
	m := dbTestManager(func(context.Context, string, ...string) (string, error) { return "", nil }, nil)
	m.Store = dbTestStore(t)
	cases := []struct {
		name   string
		path   string
		form   url.Values
		method string
		want   int
	}{
		{"backup unknown engine", "/api/databases/backup", url.Values{"engine": {"mongo"}, "name": {"shop"}}, "POST", 400},
		{"backup redis", "/api/databases/backup", url.Values{"engine": {"redis"}, "name": {"shop"}}, "POST", 400},
		{"backup bad name", "/api/databases/backup", url.Values{"engine": {"mysql"}, "name": {"bad-name"}}, "POST", 400},
		{"backup list", "/api/databases/backups", url.Values{"engine": {"mysql"}, "name": {"bad-name"}}, "GET", 400},
		{"restore bad file", "/api/databases/backup/restore", url.Values{"engine": {"mysql"}, "name": {"shop"}, "file": {"../../etc/passwd"}}, "POST", 400},
		{"restore foreign file", "/api/databases/backup/restore",
			url.Values{"engine": {"mysql"}, "name": {"shop"}, "file": {"mysql_other_20260930080910.sql.gz"}}, "POST", 400},
		{"delete bad file", "/api/databases/backup/delete", url.Values{"engine": {"mysql"}, "name": {"shop"}, "file": {"x"}}, "POST", 400},
		{"download bad file", "/api/databases/backup/download", url.Values{"engine": {"mysql"}, "name": {"shop"}, "file": {"x"}}, "GET", 404},
		{"query empty sql", "/api/databases/query", url.Values{"engine": {"mysql"}, "name": {"shop"}, "sql": {""}}, "POST", 400},
		{"query bad name", "/api/databases/query", url.Values{"engine": {"mysql"}, "name": {"bad-name"}, "sql": {"SELECT 1"}}, "POST", 400},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path+"?"+c.form.Encode(), nil)
		switch c.path {
		case "/api/databases/backup":
			m.DBBackup(rec, req)
		case "/api/databases/backups":
			m.DBBackups(rec, req)
		case "/api/databases/backup/restore":
			m.DBBackupRestore(rec, req)
		case "/api/databases/backup/delete":
			m.DBBackupDelete(rec, req)
		case "/api/databases/backup/download":
			m.DBBackupDownload(rec, req)
		default:
			m.DBQuery(rec, req)
		}
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d (body %s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}
}

func TestDatabaseBackupWithoutTaskCenterFails(t *testing.T) {
	m := dbTestManager(func(context.Context, string, ...string) (string, error) { return "", nil }, nil)
	rec := httptest.NewRecorder()
	form := url.Values{"engine": {"mysql"}, "name": {"shop"}}
	req := httptest.NewRequest("POST", "/api/databases/backup?"+form.Encode(), nil)
	m.DBBackup(rec, req)
	if rec.Code != 503 {
		t.Errorf("DBBackup without tasks status = %d, want 503", rec.Code)
	}
}

func TestLocalBackupList(t *testing.T) {
	dir := t.TempDir()
	valid := "mysql_shop_20260930080910.sql.gz"
	for _, name := range []string{valid, "mysql_other_20260930080911.sql.gz", "mysql_shop_20260930080911.sql", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list, err := localBackupList(dir, "mysql", "shop")
	if err != nil {
		t.Fatalf("localBackupList: %v", err)
	}
	if len(list) != 1 || list[0].File != valid || list[0].Size != 4 || list[0].Modified == 0 {
		t.Errorf("localBackupList = %+v", list)
	}
	if empty, err := localBackupList(filepath.Join(dir, "missing"), "mysql", "shop"); err != nil || len(empty) != 0 {
		t.Errorf("missing dir = (%v, %v), want empty list", empty, err)
	}
}

func TestDBCredentialStoreRoundtrip(t *testing.T) {
	store := dbTestStore(t)
	if _, ok := store.get("db/mysql/shop"); ok {
		t.Errorf("empty store returned a credential")
	}
	if err := store.upsert("db/mysql/shop", dbCredential{User: "shop", Password: "pw1", Host: "localhost"}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	cred, ok := store.get("db/mysql/shop")
	if !ok || cred.User != "shop" || cred.Password != "pw1" {
		t.Errorf("get = (%+v, %v)", cred, ok)
	}
	// 文件权限必须是 0600（Windows 的 chmod 只切换只读位，跳过严格断言）。
	if st, err := os.Stat(store.path); err == nil && runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode = %v, want 0600", st.Mode().Perm())
	}
	// rotatePassword 同步刷新同账号的库备注。
	if err := store.upsert("db/mysql/shop2", dbCredential{User: "shop", Password: "pw1"}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if err := store.rotatePassword("mysql", "shop", "pw2"); err != nil {
		t.Fatalf("rotatePassword: %v", err)
	}
	for _, key := range []string{"db/mysql/shop", "db/mysql/shop2"} {
		if cred, _ := store.get(key); cred.Password != "pw2" {
			t.Errorf("%s password = %q, want pw2", key, cred.Password)
		}
	}
	if cred, ok := store.get("user/mysql/shop"); !ok || cred.Password != "pw2" {
		t.Errorf("user note = (%+v, %v)", cred, ok)
	}
	// 损坏的文件按空存储处理，不会让页面崩掉。
	if err := os.WriteFile(store.path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.get("db/mysql/shop"); ok {
		t.Errorf("corrupted file returned a credential")
	}
	if err := store.upsert("db/mysql/fresh", dbCredential{Password: "x"}); err != nil {
		t.Errorf("upsert over corrupted file: %v", err)
	}
	// delete 清除条目。
	if err := store.delete("db/mysql/fresh"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := store.get("db/mysql/fresh"); ok {
		t.Errorf("deleted entry still present")
	}
}
