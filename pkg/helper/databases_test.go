package helper

import (
	"strings"
	"testing"
	"time"
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
		{"mysql", "list", "", "", "mysql", []string{"-N", "-B", "--execute=SELECT s.schema_name, s.default_character_set_name, COALESCE(SUM(t.data_length + t.index_length), 0) FROM information_schema.SCHEMATA s LEFT JOIN information_schema.TABLES t ON t.table_schema = s.schema_name GROUP BY s.schema_name, s.default_character_set_name ORDER BY s.schema_name"}, ""},
		{"mariadb", "list", "", "", "mariadb", []string{"-N", "-B", "--execute=SELECT s.schema_name, s.default_character_set_name, COALESCE(SUM(t.data_length + t.index_length), 0) FROM information_schema.SCHEMATA s LEFT JOIN information_schema.TABLES t ON t.table_schema = s.schema_name GROUP BY s.schema_name, s.default_character_set_name ORDER BY s.schema_name"}, ""},
		{"mysql", "create-db", "app_prod", "", "mysql", nil, "CREATE DATABASE `app_prod`;"},
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
		if c.wantArgs == nil && c.action == "create-db" {
			// MySQL bare create carries its statement on stdin.
			continue
		}
		if bin != c.wantBin || strings.Join(args, " ") != strings.Join(c.wantArgs, " ") || stdin != c.wantStdin {
			t.Errorf("DBCommand(%q, %q, %q) = (%q, %v, %q), want (%q, %v, %q)",
				c.engine, c.action, c.name, bin, args, stdin, c.wantBin, c.wantArgs, c.wantStdin)
		}
	}
}

func TestDBCreateCommandOneStep(t *testing.T) {
	// MySQL：一条 stdin 批处理同时建库、建号、授权；密码只进 stdin。
	bin, args, stdin, err := DBCreateCommand(DBMysql, "shop", DBCreateParams{
		User: "shop", Password: `pw's\x`, Charset: "utf8mb4", Host: "10.0.%.%",
	})
	if err != nil {
		t.Fatalf("DBCreateCommand(mysql) error: %v", err)
	}
	if bin != "mysql" || len(args) != 0 {
		t.Errorf("DBCreateCommand(mysql) = (%q, %v), want (mysql, [])", bin, args)
	}
	for _, want := range []string{
		"CREATE DATABASE `shop` CHARACTER SET utf8mb4;",
		"CREATE USER 'shop'@'10.0.%.%' IDENTIFIED BY 'pw\\'s\\\\x';",
		"GRANT ALL PRIVILEGES ON `shop`.* TO 'shop'@'10.0.%.%';",
	} {
		if !strings.Contains(stdin, want) {
			t.Errorf("stdin missing fragment %q:\n%s", want, stdin)
		}
	}
	if strings.Contains(strings.Join(args, " "), "pw") {
		t.Errorf("password leaked into argv %v", args)
	}
	// 默认 host 是 localhost。
	_, _, stdin, err = DBCreateCommand(DBMariadb, "shop", DBCreateParams{User: "u1", Password: "pw"})
	if err != nil || !strings.Contains(stdin, "'u1'@'localhost'") {
		t.Errorf("DBCreateCommand(mariadb) stdin = %q, err %v, want localhost account", stdin, err)
	}
	// PostgreSQL：建号 + 建库一条 psql 批处理，OWNER 直接授权。
	bin, args, stdin, err = DBCreateCommand(DBPostgresql, "shop", DBCreateParams{User: "shop", Password: "it's"})
	if err != nil {
		t.Fatalf("DBCreateCommand(postgresql) error: %v", err)
	}
	if bin != "runuser" || strings.Join(args, " ") != "-u postgres -- psql -qAt -v ON_ERROR_STOP=1" {
		t.Errorf("DBCreateCommand(postgresql) = (%q, %v)", bin, args)
	}
	for _, want := range []string{`CREATE ROLE "shop" WITH LOGIN PASSWORD 'it''s';`, `CREATE DATABASE "shop" OWNER "shop";`} {
		if !strings.Contains(stdin, want) {
			t.Errorf("pg stdin missing %q:\n%s", want, stdin)
		}
	}
}

func TestDBCreateCommandRejections(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		params DBCreateParams
	}{
		{"bad charset", DBMysql, DBCreateParams{Charset: "utf8mb4; DROP"}},
		{"charset on postgresql", DBPostgresql, DBCreateParams{Charset: "UTF8"}},
		{"bad user", DBMysql, DBCreateParams{User: "bad user", Password: "pw"}},
		{"missing password", DBMysql, DBCreateParams{User: "app_user"}},
		{"bad host", DBMysql, DBCreateParams{User: "app_user", Password: "pw", Host: "evil'host"}},
		{"bad name", DBMysql, DBCreateParams{User: "app_user", Password: "pw"}},
	}
	for _, c := range cases {
		name := c.params.User
		if c.name == "bad name" {
			name = "bad-name"
		}
		if _, _, _, err := DBCreateCommand(c.engine, name, c.params); err == nil {
			t.Errorf("%s: DBCreateCommand accepted, want rejection", c.name)
		}
	}
}

func TestDBBackupRestoreQueryCommands(t *testing.T) {
	if bin, args, err := DBBackupCommand(DBMysql, "shop"); err != nil || bin != "mysqldump" ||
		strings.Join(args, " ") != "--single-transaction --quick --routines --events shop" {
		t.Errorf("DBBackupCommand(mysql) = (%q, %v, %v)", bin, args, err)
	}
	if bin, args, err := DBBackupCommand(DBMariadb, "shop"); err != nil || bin != "mariadb-dump" {
		t.Errorf("DBBackupCommand(mariadb) = (%q, %v, %v)", bin, args, err)
	}
	if bin, args, err := DBBackupCommand(DBPostgresql, "shop"); err != nil || bin != "runuser" ||
		strings.Join(args, " ") != "-u postgres -- pg_dump shop" {
		t.Errorf("DBBackupCommand(postgresql) = (%q, %v, %v)", bin, args, err)
	}
	if bin, args, err := DBRestoreCommand(DBMysql, "shop"); err != nil || bin != "mysql" || strings.Join(args, " ") != "shop" {
		t.Errorf("DBRestoreCommand(mysql) = (%q, %v, %v)", bin, args, err)
	}
	if bin, args, err := DBRestoreCommand(DBPostgresql, "shop"); err != nil ||
		!strings.Contains(strings.Join(args, " "), "ON_ERROR_STOP=1") {
		t.Errorf("DBRestoreCommand(postgresql) = (%q, %v, %v)", bin, args, err)
	}
	if bin, args, err := DBQueryCommand(DBMysql, "shop"); err != nil || bin != "mysql" || strings.Join(args, " ") != "--batch shop" {
		t.Errorf("DBQueryCommand(mysql) = (%q, %v, %v)", bin, args, err)
	}
	if _, _, err := DBQueryCommand(DBRedis, "shop"); err == nil {
		t.Errorf("DBQueryCommand(redis) accepted, want rejection")
	}
	// dump 工具按自己的候选列表解析，而不是复用客户端列表。
	got, err := ResolveDBBinary(DBMariadb, "mariadb-dump", func(n string) bool { return n == "mysqldump" })
	if err != nil || got != "mysqldump" {
		t.Errorf("ResolveDBBinary(mariadb-dump fallback) = (%q, %v)", got, err)
	}
	if _, err := ResolveDBBinary(DBMysql, "mysqldump", func(string) bool { return false }); err == nil {
		t.Errorf("ResolveDBBinary(mysqldump, nothing installed) accepted, want error")
	}
}

func TestBackupFileNames(t *testing.T) {
	file, err := BackupFileName(DBMysql, "shop_prod", time.Date(2026, 9, 30, 8, 9, 10, 0, time.Local))
	if err != nil || file != "mysql_shop_prod_20260930080910.sql.gz" {
		t.Fatalf("BackupFileName = (%q, %v)", file, err)
	}
	engine, name, ts, ok := ParseBackupFile(file)
	if !ok || engine != DBMysql || name != "shop_prod" || ts.Format(dbBackupTimeLayout) != "20260930080910" {
		t.Errorf("ParseBackupFile = (%q, %q, %v, %v)", engine, name, ts, ok)
	}
	if !ValidBackupFile(DBMysql, "shop_prod", file) {
		t.Errorf("ValidBackupFile(%q) = false, want true", file)
	}
	for _, bad := range []string{
		"", "mysql_shop_prod.sql.gz", "mysql_shop_prod_20260930080910.sql",
		"../mysql_shop_prod_20260930080910.sql.gz", "mongo_shop_20260930080910.sql.gz",
		"mysql_.._20260930080910.sql.gz", "mysql_shop_2026093008091x.sql.gz",
		"mysql_shop_prod_20260930080910.sql.gz.bak", "/etc/passwd",
	} {
		if ValidBackupFile(DBMysql, "shop_prod", bad) {
			t.Errorf("ValidBackupFile(%q) = true, want false", bad)
		}
	}
	// 备份名与其它库同名前缀的文件不会串。
	if ValidBackupFile(DBMysql, "shop", file) {
		t.Errorf("ValidBackupFile(other database) accepted")
	}
}

func TestDBValidators(t *testing.T) {
	if !ValidDBCharset(DBMysql, "utf8mb4") || ValidDBCharset(DBMysql, "") == false || ValidDBCharset(DBMysql, "utf8mb4'") {
		t.Errorf("ValidDBCharset(mysql) misbehaves")
	}
	if ValidDBCharset(DBPostgresql, "UTF8") {
		t.Errorf("ValidDBCharset(postgresql) must reject explicit charsets")
	}
	for _, host := range []string{"localhost", "%", "10.0.%.%", "db1.example.com", "192.168.1.5"} {
		if !ValidDBHost(host) {
			t.Errorf("ValidDBHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"", "evil'host", "host;drop", strings.Repeat("a", 256)} {
		if ValidDBHost(host) {
			t.Errorf("ValidDBHost(%q) = true, want false", host)
		}
	}
	if !ValidDBSQL("SELECT 1;\nUPDATE t SET x=1;") {
		t.Errorf("ValidDBSQL rejects a plain batch")
	}
	for _, sql := range []string{"", strings.Repeat("a", MaxDBSQL+1), "SELECT '\x00'"} {
		if ValidDBSQL(sql) {
			t.Errorf("ValidDBSQL accepted %d-byte/NUL input, want false", len(sql))
		}
	}
}

func TestParseDBInfoRow(t *testing.T) {
	info, ok := ParseDBInfoRow("shop\tutf8mb4\t1048576")
	if !ok || info.Name != "shop" || info.Charset != "utf8mb4" || info.Size != 1048576 {
		t.Errorf("ParseDBInfoRow(tsv) = %+v, %v", info, ok)
	}
	info, ok = ParseDBInfoRow("shop\tutf8mb4\t(bad)")
	if !ok || info.Size != 0 {
		t.Errorf("ParseDBInfoRow(bad size) = %+v, %v", info, ok)
	}
	info, ok = ParseDBInfoRow("plain_name")
	if !ok || info.Name != "plain_name" || info.Charset != "" {
		t.Errorf("ParseDBInfoRow(legacy) = %+v, %v", info, ok)
	}
	if _, ok = ParseDBInfoRow("\tutf8mb4\t1"); ok {
		t.Errorf("ParseDBInfoRow(empty name) accepted")
	}
	if rows := ParseDBList([]string{"a\tutf8\t5", "b", "", "c\tlatin1\t0"}); len(rows) != 3 {
		t.Errorf("ParseDBList = %+v, want 3 rows", rows)
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
