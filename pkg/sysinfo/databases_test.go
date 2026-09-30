package sysinfo

import (
	"context"
	"net/http/httptest"
	"net/url"
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
	}
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
				if strings.Contains(arg, "SHOW DATABASES") {
					return "information_schema\nshop\nmysql\n", nil
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
	if !strings.Contains(body, `"shop"`) {
		t.Errorf("user database missing: %s", body)
	}
	if strings.Contains(body, `"information_schema"`) || strings.Contains(body, `"performance_schema"`) {
		t.Errorf("system databases must be filtered: %s", body)
	}
	if !strings.Contains(body, `app_user`) || strings.Contains(body, `mysql.sys`) {
		t.Errorf("user listing not filtered correctly: %s", body)
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
