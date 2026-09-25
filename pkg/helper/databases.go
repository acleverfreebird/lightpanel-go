package helper

import (
	"fmt"
	"strings"
)

// Database management: the supported engines, their service units and the
// exact argv built for every database operation. Both the panel and the
// helper compile this file, so they agree on engine keys, validators and on
// the commands built for each action — the panel only ever sends the engine,
// a validated name and (for user operations) the password, and the helper
// recomputes everything else. Passwords travel in the Request and are fed to
// the client binary over stdin, so they never appear in a process list.

// Database engines. mysql/mariadb/postgresql support the full operation set;
// redis is detected and reported (its state is managed through the services
// page) but has no database/user operations.
const (
	DBMysql      = "mysql"
	DBMariadb    = "mariadb"
	DBPostgresql = "postgresql"
	DBRedis      = "redis"
)

var dbEngines = map[string]bool{DBMysql: true, DBMariadb: true, DBPostgresql: true, DBRedis: true}

// ValidDBEngine reports whether engine is a supported database engine key.
func ValidDBEngine(engine string) bool { return dbEngines[engine] }

// DBEngineUnits maps each engine to its systemd unit candidates, in probing
// order. The panel probes them with `systemctl is-active`.
var DBEngineUnits = map[string][]string{
	DBMysql:      {"mysql.service", "mysqld.service"},
	DBMariadb:    {"mariadb.service", "mariadbd.service", "mysql.service"},
	DBPostgresql: {"postgresql.service", "postgresql@*.service"},
	DBRedis:      {"redis-server.service", "redis.service"},
}

// DBServerBinaries maps each engine to its server binary candidates, in
// probing order, used for version detection (`<bin> --version`).
var DBServerBinaries = map[string][]string{
	DBMysql:      {"mysqld", "mariadbd"},
	DBMariadb:    {"mariadbd", "mysqld"},
	DBPostgresql: {"postgres", "psql"},
	DBRedis:      {"redis-server"},
}

// DBClientBinaries maps each SQL engine to its client binary candidates, in
// preference order. redis has no SQL client; operations on it are rejected.
var DBClientBinaries = map[string][]string{
	DBMysql:      {"mysql"},
	DBMariadb:    {"mariadb", "mysql"},
	DBPostgresql: {"psql"},
}

// ValidDBName reports whether name may be used as a database name. Names are
// interpolated into SQL with backtick quoting, so the alphabet stays closed:
// alphanumerics and underscores only, which need no escaping inside backticks.
func ValidDBName(name string) bool {
	if len(name) == 0 || len(name) > 63 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// ValidDBUser reports whether name may be used as a database user name. The
// first character is alphanumeric/underscore so the name can never be
// mistaken for a command-line flag.
func ValidDBUser(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_':
		case i > 0 && (c == '.' || c == '-'):
		default:
			return false
		}
	}
	return true
}

// ValidDBPassword reports whether password is acceptable for CREATE USER /
// ALTER ROLE. It must be non-empty, at most 128 bytes and free of control
// characters — everything else (quotes, backslashes, spaces, unicode) is
// allowed because the SQL statement is rendered with proper escaping and fed
// over stdin, never argv.
func ValidDBPassword(password string) bool {
	if len(password) == 0 || len(password) > 128 {
		return false
	}
	for _, c := range password {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// escapeMysqlLiteral renders s as the inside of a single-quoted MySQL string
// literal (backslash escaping).
func escapeMysqlLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

// escapePGLiteral renders s as the inside of a single-quoted PostgreSQL
// string literal (doubled quotes; backslashes stay literal under
// standard_conforming_strings, which is the default on every supported
// release).
func escapePGLiteral(s string) string {
	return strings.ReplaceAll(s, `'`, `''`)
}

// DBCommand builds the complete argv for one validated database operation.
// bin is the first-choice binary (callers substitute the first available
// candidate from DBClientBinaries when it is missing); for user operations
// stdin carries the rendered SQL (including the password) and the argv never
// contains it. PostgreSQL runs its client as the postgres system user.
func DBCommand(engine, action, name, password string) (bin string, args []string, stdin string, err error) {
	if !ValidDBEngine(engine) {
		return "", nil, "", fmt.Errorf("unsupported database engine %q", engine)
	}
	if engine == DBRedis {
		return "", nil, "", fmt.Errorf("redis does not support database or user management")
	}
	switch action {
	case "list":
		return dbListCommand(engine)
	case "create-db", "drop-db":
		if !ValidDBName(name) {
			return "", nil, "", fmt.Errorf("invalid database name")
		}
		if engine == DBPostgresql {
			cmd := "createdb"
			if action == "drop-db" {
				cmd = "dropdb"
				// --if-exists turns a double delete into a plain message;
				// supported by every PostgreSQL release in distro repos.
				return "runuser", []string{"-u", "postgres", "--", cmd, "--if-exists", name}, "", nil
			}
			return "runuser", []string{"-u", "postgres", "--", cmd, name}, "", nil
		}
		stmt := "CREATE DATABASE `" + name + "`"
		if action == "drop-db" {
			stmt = "DROP DATABASE `" + name + "`"
		}
		return dbClient(engine), []string{"--execute=" + stmt}, "", nil
	case "create-user", "set-password":
		if !ValidDBUser(name) {
			return "", nil, "", fmt.Errorf("invalid user name")
		}
		if !ValidDBPassword(password) {
			return "", nil, "", fmt.Errorf("invalid password")
		}
		if engine == DBPostgresql {
			stmt := fmt.Sprintf("CREATE ROLE %q WITH LOGIN PASSWORD '%s';", name, escapePGLiteral(password))
			if action == "set-password" {
				stmt = fmt.Sprintf("ALTER ROLE %q WITH PASSWORD '%s';", name, escapePGLiteral(password))
			}
			return "runuser", []string{"-u", "postgres", "--", "psql", "-qAt"}, stmt, nil
		}
		stmt := fmt.Sprintf("CREATE USER '%s'@'localhost' IDENTIFIED BY '%s';", name, escapeMysqlLiteral(password))
		if action == "set-password" {
			stmt = fmt.Sprintf("ALTER USER '%s'@'localhost' IDENTIFIED BY '%s';", name, escapeMysqlLiteral(password))
		}
		return dbClient(engine), nil, stmt, nil
	}
	return "", nil, "", fmt.Errorf("unsupported database action %q", action)
}

func dbListCommand(engine string) (string, []string, string, error) {
	if engine == DBPostgresql {
		return "runuser", []string{"-u", "postgres", "--", "psql", "-At", "-c",
			"SELECT datname FROM pg_database WHERE NOT datistemplate ORDER BY 1"}, "", nil
	}
	return dbClient(engine), []string{"-N", "-B", "--execute=SHOW DATABASES"}, "", nil
}

// DBUserCommand builds the argv to list database users (one per line, MySQL
// rows rendered as user@host). Engines without user listing return an error.
func DBUserCommand(engine string) (bin string, args []string, err error) {
	if !ValidDBEngine(engine) {
		return "", nil, fmt.Errorf("unsupported database engine %q", engine)
	}
	if engine == DBRedis {
		return "", nil, fmt.Errorf("redis does not support database or user management")
	}
	if engine == DBPostgresql {
		return "runuser", []string{"-u", "postgres", "--", "psql", "-At", "-c",
			"SELECT rolname FROM pg_roles WHERE rolcanlogin ORDER BY 1"}, nil
	}
	return dbClient(engine), []string{"-N", "-B", "--execute=SELECT User, Host FROM mysql.user ORDER BY User"}, nil
}

// dbClient returns the first-choice SQL client binary for a MySQL-family
// engine. Callers fall back through DBClientBinaries when it is absent.
func dbClient(engine string) string {
	if engine == DBMariadb {
		return "mariadb"
	}
	return "mysql"
}

// ResolveDBBinary picks the first available binary for a command built by
// DBCommand/DBUserCommand: runuser is resolved directly, otherwise the
// engine's client candidates are tried in preference order. Both ends use
// this so the panel and the helper agree on substitution. available reports
// whether a whitelisted binary exists (without executing it).
func ResolveDBBinary(engine, bin string, available func(string) bool) (string, error) {
	if bin == "runuser" {
		if available("runuser") {
			return "runuser", nil
		}
		return "", fmt.Errorf("required system utility runuser unavailable")
	}
	for _, candidate := range DBClientBinaries[engine] {
		if available(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no SQL client found for engine %s", engine)
}
