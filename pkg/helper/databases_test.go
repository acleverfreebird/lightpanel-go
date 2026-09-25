package helper

import (
	"strings"
	"testing"
)

func TestValidDBEngine(t *testing.T) {
	for _, engine := range []string{DBMysql, DBMariadb, DBPostgresql, DBRedis} {
		if !ValidDBEngine(engine) {
			t.Errorf("ValidDBEngine(%q) = false, want true", engine)
		}
	}
	for _, engine := range []string{"", "mysql ", "MySQL", "mongo", "mssql", "mysql;rm"} {
		if ValidDBEngine(engine) {
			t.Errorf("ValidDBEngine(%q) = true, want false", engine)
		}
	}
}

func TestValidDBName(t *testing.T) {
	for _, name := range []string{"app", "app_production", "App123", "a", strings.Repeat("a", 63)} {
		if !ValidDBName(name) {
			t.Errorf("ValidDBName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "app-prod", "app.prod", "app prod", "app;drop", "`app`", strings.Repeat("a", 64)} {
		if ValidDBName(name) {
			t.Errorf("ValidDBName(%q) = true, want false", name)
		}
	}
}

func TestValidDBUser(t *testing.T) {
	for _, name := range []string{"app_user", "app.user", "app-user", "App9", "a"} {
		if !ValidDBUser(name) {
			t.Errorf("ValidDBUser(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", ".hidden", "-lead", "user name", "us'er", "us\"er", strings.Repeat("a", 33)} {
		if ValidDBUser(name) {
			t.Errorf("ValidDBUser(%q) = true, want false", name)
		}
	}
}

func TestValidDBPassword(t *testing.T) {
	for _, password := range []string{"simple", "with space", `qu'ote`, `back\slash`, "ünïcodé", strings.Repeat("a", 128)} {
		if !ValidDBPassword(password) {
			t.Errorf("ValidDBPassword(%q) = false, want true", password)
		}
	}
	for _, password := range []string{"", "new\nline", "carriage\rreturn", "null\x00byte", "tab\there", strings.Repeat("a", 129)} {
		if ValidDBPassword(password) {
			t.Errorf("ValidDBPassword(%q) = true, want false", password)
		}
	}
}

func TestDBCommand(t *testing.T) {
	cases := []struct {
		engine, action, name, password string
		wantBin                        string
		wantArgs                       []string
		wantStdin                      string
	}{
		{"mysql", "list", "", "", "mysql", []string{"-N", "-B", "--execute=SHOW DATABASES"}, ""},
		{"mariadb", "list", "", "", "mariadb", []string{"-N", "-B", "--execute=SHOW DATABASES"}, ""},
		{"mysql", "create-db", "app_prod", "", "mysql", []string{"--execute=CREATE DATABASE `app_prod`"}, ""},
		{"mysql", "drop-db", "app_prod", "", "mysql", []string{"--execute=DROP DATABASE `app_prod`"}, ""},
		{"postgresql", "create-db", "app_prod", "", "runuser", []string{"-u", "postgres", "--", "createdb", "app_prod"}, ""},
		{"postgresql", "drop-db", "app_prod", "", "runuser", []string{"-u", "postgres", "--", "dropdb", "--if-exists", "app_prod"}, ""},
	}
	for _, c := range cases {
		bin, args, stdin, err := DBCommand(c.engine, c.action, c.name, c.password)
		if err != nil {
			t.Errorf("DBCommand(%q, %q): unexpected error %v", c.engine, c.action, err)
			continue
		}
		if bin != c.wantBin || strings.Join(args, " ") != strings.Join(c.wantArgs, " ") || stdin != c.wantStdin {
			t.Errorf("DBCommand(%q, %q, %q) = (%q, %v, %q), want (%q, %v, %q)",
				c.engine, c.action, c.name, bin, args, stdin, c.wantBin, c.wantArgs, c.wantStdin)
		}
	}
}

func TestDBCommandUserStatements(t *testing.T) {
	cases := []struct {
		engine, action, name, password, wantFragment string
	}{
		{"mysql", "create-user", "app_user", `pw's"x`, "CREATE USER 'app_user'@'localhost' IDENTIFIED BY 'pw\\'s\"x';"},
		{"mariadb", "create-user", "app_user", `back\slash`, "IDENTIFIED BY 'back\\\\slash';"},
		{"mysql", "set-password", "app_user", "secret", "ALTER USER 'app_user'@'localhost' IDENTIFIED BY 'secret';"},
		{"postgresql", "create-user", "app_user", "it's", `CREATE ROLE "app_user" WITH LOGIN PASSWORD 'it''s';`},
		{"postgresql", "set-password", "app_user", "secret", `ALTER ROLE "app_user" WITH PASSWORD 'secret';`},
	}
	for _, c := range cases {
		bin, args, stdin, err := DBCommand(c.engine, c.action, c.name, c.password)
		if err != nil {
			t.Errorf("DBCommand(%q, %q): unexpected error %v", c.engine, c.action, err)
			continue
		}
		if !strings.Contains(stdin, c.wantFragment) {
			t.Errorf("DBCommand(%q, %q) stdin = %q, want fragment %q", c.engine, c.action, stdin, c.wantFragment)
		}
		// The password must never reach argv: it is rendered into stdin only.
		for _, arg := range args {
			if strings.Contains(arg, c.password) {
				t.Errorf("DBCommand(%q, %q): password leaked into argv %q", c.engine, c.action, arg)
			}
		}
		if c.engine == DBPostgresql && (len(args) < 2 || args[0] != "-u" || args[1] != "postgres") {
			t.Errorf("DBCommand(postgresql, %q) args = %v, want runuser postgres invocation", c.action, args)
		}
		_ = bin
	}
}

func TestDBCommandRejections(t *testing.T) {
	cases := []struct{ engine, action, name, password string }{
		{"redis", "list", "", ""},
		{"redis", "create-db", "app", ""},
		{"mongo", "list", "", ""},
		{"mysql", "reboot", "", ""},
		{"mysql", "create-db", "bad-name", ""},
		{"mysql", "create-db", "", ""},
		{"mysql", "create-user", "bad name", "secret"},
		{"mysql", "create-user", "app_user", ""},
		{"mysql", "create-user", "app_user", "bad\npassword"},
		{"postgresql", "set-password", "app_user", strings.Repeat("a", 129)},
	}
	for _, c := range cases {
		if _, _, _, err := DBCommand(c.engine, c.action, c.name, c.password); err == nil {
			t.Errorf("DBCommand(%q, %q, %q) accepted, want rejection", c.engine, c.action, c.name)
		}
	}
}

func TestDBUserCommand(t *testing.T) {
	if bin, args, err := DBUserCommand(DBMysql); err != nil || bin != "mysql" ||
		strings.Join(args, " ") != "-N -B --execute=SELECT User, Host FROM mysql.user ORDER BY User" {
		t.Errorf("DBUserCommand(mysql) = (%q, %v, %v)", bin, args, err)
	}
	if bin, args, err := DBUserCommand(DBPostgresql); err != nil || bin != "runuser" || args[1] != "postgres" {
		t.Errorf("DBUserCommand(postgresql) = (%q, %v, %v)", bin, args, err)
	}
	if _, _, err := DBUserCommand(DBRedis); err == nil {
		t.Errorf("DBUserCommand(redis) accepted, want rejection")
	}
}

func TestResolveDBBinary(t *testing.T) {
	available := map[string]bool{"mariadb": true, "mysql": true}
	got, err := ResolveDBBinary(DBMariadb, "mariadb", func(n string) bool { return available[n] })
	if err != nil || got != "mariadb" {
		t.Errorf("ResolveDBBinary(mariadb) = (%q, %v)", got, err)
	}
	// Only the generic mysql client exists: the mariadb engine falls back.
	delete(available, "mariadb")
	got, err = ResolveDBBinary(DBMariadb, "mariadb", func(n string) bool { return available[n] })
	if err != nil || got != "mysql" {
		t.Errorf("ResolveDBBinary(mariadb fallback) = (%q, %v)", got, err)
	}
	// PostgreSQL runs its client through runuser.
	got, err = ResolveDBBinary(DBPostgresql, "runuser", func(n string) bool { return n == "runuser" })
	if err != nil || got != "runuser" {
		t.Errorf("ResolveDBBinary(postgresql) = (%q, %v)", got, err)
	}
	if _, err := ResolveDBBinary(DBMysql, "mysql", func(string) bool { return false }); err == nil {
		t.Errorf("ResolveDBBinary(mysql, nothing installed) accepted, want error")
	}
}
