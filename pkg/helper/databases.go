package helper

import (
	"compress/gzip"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
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

// dbCharsets is the whitelist of character sets offered for database
// creation, per engine. The alphabet is closed because the charset is
// interpolated into SQL unquoted. PostgreSQL is absent: switching its
// encoding needs template0 plus a matching locale, so the panel keeps the
// engine default there and any charset request is rejected.
var dbCharsets = map[string]map[string]bool{
	DBMysql:      {"utf8mb4": true, "utf8": true, "latin1": true, "gbk": true, "big5": true},
	DBMariadb:    {"utf8mb4": true, "utf8": true, "latin1": true, "gbk": true, "big5": true},
	DBPostgresql: nil,
}

// ValidDBCharset reports whether charset may be used as the CHARACTER SET of
// a new database on engine. Empty means "engine default" and always passes.
func ValidDBCharset(engine, charset string) bool {
	if charset == "" {
		return true
	}
	return dbCharsets[engine][charset]
}

// ValidDBHost reports whether host may be used as the account host part of a
// MySQL user ('user'@'host'). The value is also escaped when rendered into
// SQL, so this alphabet check is a hard boundary, not the only defense.
// Empty means localhost and is normalized by the caller.
func ValidDBHost(host string) bool {
	if len(host) == 0 || len(host) > 255 {
		return false
	}
	for _, c := range host {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '%' || c == ':' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// MaxDBSQL caps one SQL console batch. It must stay well below the helper's
// requestLimit, which bounds the whole JSON request.
const MaxDBSQL = 32 << 10

// ValidDBSQL reports whether sql is acceptable for the query action: it is
// fed to the client over stdin, so everything except NUL and size is allowed
// — the operator already holds a root web terminal, arbitrary SQL adds no
// privilege.
func ValidDBSQL(sql string) bool {
	if len(sql) == 0 || len(sql) > MaxDBSQL {
		return false
	}
	return !strings.ContainsRune(sql, 0)
}

// DBBackupDir is the fixed, helper-managed backup directory. Backups are
// named <engine>_<database>_<timestamp>.sql.gz, written 0600; in helper mode
// the file is chowned to the connecting panel user so the unprivileged panel
// process can stream downloads while the directory itself stays root-owned.
const DBBackupDir = "/var/backups/lightpanel/databases"

const dbBackupSuffix = ".sql.gz"
const dbBackupTimeLayout = "20060102150405"

// BackupFileName renders the managed backup file name for one database. Both
// parts are validated so the name can never carry path components.
func BackupFileName(engine, name string, t time.Time) (string, error) {
	if !ValidDBEngine(engine) {
		return "", fmt.Errorf("unsupported database engine %q", engine)
	}
	if !ValidDBName(name) {
		return "", fmt.Errorf("invalid database name")
	}
	return engine + "_" + name + "_" + t.Format(dbBackupTimeLayout) + dbBackupSuffix, nil
}

// ParseBackupFile splits a managed backup file name back into its parts. The
// timestamp is split from the END because database names may contain
// underscores; the engine cannot. Both parts are re-validated, so a parsed
// name is safe to join onto DBBackupDir.
func ParseBackupFile(file string) (engine, name string, t time.Time, ok bool) {
	if !strings.HasSuffix(file, dbBackupSuffix) || strings.ContainsAny(file, `/\`) {
		return "", "", time.Time{}, false
	}
	body := strings.TrimSuffix(file, dbBackupSuffix)
	i := strings.LastIndex(body, "_")
	j := strings.Index(body, "_")
	if i < 0 || j < 0 || i == j {
		return "", "", time.Time{}, false
	}
	ts := body[i+1:]
	if len(ts) != len(dbBackupTimeLayout) {
		return "", "", time.Time{}, false
	}
	parsed, err := time.ParseInLocation(dbBackupTimeLayout, ts, time.Local)
	if err != nil {
		return "", "", time.Time{}, false
	}
	engine, name = body[:j], body[j+1:i]
	if !ValidDBEngine(engine) || !ValidDBName(name) {
		return "", "", time.Time{}, false
	}
	return engine, name, parsed, true
}

// ValidBackupFile reports whether file names a managed backup of exactly the
// given engine/database pair.
func ValidBackupFile(engine, name, file string) bool {
	e, n, _, ok := ParseBackupFile(file)
	return ok && e == engine && n == name
}

// DumpGzipWriter wraps a dump file writer in a gzip stream (fastest level:
// large dumps are I/O bound and compression is a convenience, not an archive
// format). The caller must Close it before closing the file.
func DumpGzipWriter(w io.Writer) *gzip.Writer {
	g, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		g = gzip.NewWriter(w)
	}
	return g
}

// DumpGzipReader opens a stream written by DumpGzipWriter.
func DumpGzipReader(r io.Reader) (*gzip.Reader, error) {
	return gzip.NewReader(r)
}

// DBInfo is one row of the engine's database listing: the name plus the live
// size and default charset where the engine reports them.
type DBInfo struct {
	Name    string `json:"name"`
	Charset string `json:"charset,omitempty"`
	Size    uint64 `json:"size"` // 0 对空库是真实值，不用 omitempty
}

// ParseDBInfoRow decodes one TSV row of the list command's output
// (name, charset, size). A plain single-field row (from a pre-v4 helper or a
// stub) degrades to a name-only entry.
func ParseDBInfoRow(line string) (DBInfo, bool) {
	line = strings.TrimRight(line, "\r")
	parts := strings.Split(line, "\t")
	name := strings.TrimSpace(parts[0])
	if name == "" {
		return DBInfo{}, false
	}
	info := DBInfo{Name: name}
	if len(parts) >= 2 {
		info.Charset = strings.TrimSpace(parts[1])
	}
	if len(parts) >= 3 {
		if n, err := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 64); err == nil {
			info.Size = n
		}
	}
	return info, true
}

// ParseDBList decodes every row of the list output into DBInfo entries.
func ParseDBList(rows []string) []DBInfo {
	out := make([]DBInfo, 0, len(rows))
	for _, row := range rows {
		if info, ok := ParseDBInfoRow(row); ok {
			out = append(out, info)
		}
	}
	return out
}

// DBListPayload is the JSON shape of the list action's Output, shared by the
// helper (which marshals it) and the panel (which decodes it).
type DBListPayload struct {
	Databases []DBInfo `json:"databases"`
	Users     []string `json:"users"`
}

// DBCreateParams carries the optional one-step provisioning parameters of
// the create-db action: creating the database and its matching account in
// one shot, the way the databases page offers it.
type DBCreateParams struct {
	User     string // account name; empty = database only (no account)
	Password string
	Charset  string // CHARACTER SET for MySQL-family engines; empty = default
	Host     string // MySQL account host; empty = localhost
}

// DBCreateCommand builds the complete argv (and stdin) for creating one
// database, optionally provisioning its account in the same step. With an
// account the MySQL family receives one batch of SQL over stdin (CREATE
// DATABASE, CREATE USER, GRANT); PostgreSQL receives createdb when bare and
// one psql batch (CREATE ROLE + CREATE DATABASE ... OWNER) otherwise — psql
// runs with ON_ERROR_STOP so a failed role never leaves a half-provisioned
// database behind. Everything is re-validated here; the caller contributes
// only validated form fields.
func DBCreateCommand(engine, name string, p DBCreateParams) (bin string, args []string, stdin string, err error) {
	if !ValidDBEngine(engine) {
		return "", nil, "", fmt.Errorf("unsupported database engine %q", engine)
	}
	if engine == DBRedis {
		return "", nil, "", fmt.Errorf("redis does not support database or user management")
	}
	if !ValidDBName(name) {
		return "", nil, "", fmt.Errorf("invalid database name")
	}
	if p.Charset != "" && !ValidDBCharset(engine, p.Charset) {
		return "", nil, "", fmt.Errorf("unsupported character set %q for engine %s", p.Charset, engine)
	}
	if p.User != "" {
		if !ValidDBUser(p.User) {
			return "", nil, "", fmt.Errorf("invalid user name")
		}
		if !ValidDBPassword(p.Password) {
			return "", nil, "", fmt.Errorf("invalid password")
		}
	}
	host := p.Host
	if host == "" {
		host = "localhost"
	} else if !ValidDBHost(host) {
		return "", nil, "", fmt.Errorf("invalid access host")
	}
	if engine == DBPostgresql {
		if p.User == "" {
			return "runuser", []string{"-u", "postgres", "--", "createdb", name}, "", nil
		}
		stmt := `CREATE ROLE "` + p.User + `" WITH LOGIN PASSWORD '` + escapePGLiteral(p.Password) + `';` + "\n" +
			`CREATE DATABASE "` + name + `" OWNER "` + p.User + `";`
		return "runuser", []string{"-u", "postgres", "--", "psql", "-qAt", "-v", "ON_ERROR_STOP=1"}, stmt, nil
	}
	var b strings.Builder
	b.WriteString("CREATE DATABASE `" + name + "`")
	if p.Charset != "" {
		b.WriteString(" CHARACTER SET " + p.Charset)
	}
	if p.User != "" {
		b.WriteString(";\nCREATE USER '" + p.User + "'@'" + host + "' IDENTIFIED BY '" + escapeMysqlLiteral(p.Password) + "'")
		b.WriteString(";\nGRANT ALL PRIVILEGES ON `" + name + "`.* TO '" + p.User + "'@'" + host + "'")
	}
	b.WriteString(";")
	return dbClient(engine), nil, b.String(), nil
}

// DBCommand builds the complete argv for one validated database operation.
// bin is the first-choice binary (callers substitute the first available
// candidate from DBClientBinaries/DBDumpBinaries when it is missing); for
// user operations stdin carries the rendered SQL (including the password) and
// the argv never contains it. PostgreSQL runs its client as the postgres
// system user.
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
	case "create-db":
		return DBCreateCommand(engine, name, DBCreateParams{})
	case "drop-db":
		if !ValidDBName(name) {
			return "", nil, "", fmt.Errorf("invalid database name")
		}
		if engine == DBPostgresql {
			// --if-exists turns a double delete into a plain message;
			// supported by every PostgreSQL release in distro repos.
			return "runuser", []string{"-u", "postgres", "--", "dropdb", "--if-exists", name}, "", nil
		}
		return dbClient(engine), []string{"--execute=DROP DATABASE `" + name + "`"}, "", nil
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
		return "runuser", []string{"-u", "postgres", "--", "psql", "-A", "-t", "-F", "\t", "-c",
			"SELECT d.datname, pg_encoding_to_char(d.encoding), pg_database_size(d.datname) FROM pg_database d WHERE NOT d.datistemplate ORDER BY 1"}, "", nil
	}
	return dbClient(engine), []string{"-N", "-B", "--execute=SELECT s.schema_name, s.default_character_set_name, COALESCE(SUM(t.data_length + t.index_length), 0) FROM information_schema.SCHEMATA s LEFT JOIN information_schema.TABLES t ON t.table_schema = s.schema_name GROUP BY s.schema_name, s.default_character_set_name ORDER BY s.schema_name"}, "", nil
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

// DBDumpBinaries maps each SQL engine to its dump binary candidates, in
// preference order. MariaDB ships the dumper under its own name with a
// mysqldump alias.
var DBDumpBinaries = map[string][]string{
	DBMysql:      {"mysqldump"},
	DBMariadb:    {"mariadb-dump", "mysqldump"},
	DBPostgresql: {"pg_dump"},
}

// dbDumpCandidates maps a dump command's first-choice binary to its candidate
// list, so ResolveDBBinary substitutes within the right family.
var dbDumpCandidates = map[string][]string{
	"mysqldump":    DBDumpBinaries[DBMysql],
	"mariadb-dump": DBDumpBinaries[DBMariadb],
	"pg_dump":      DBDumpBinaries[DBPostgresql],
}

// DBBackupCommand builds the argv for a full dump of one database. The caller
// streams stdout into the gzipped backup file — the dump is never buffered.
func DBBackupCommand(engine, name string) (bin string, args []string, err error) {
	if !ValidDBEngine(engine) || engine == DBRedis {
		return "", nil, fmt.Errorf("unsupported database engine %q", engine)
	}
	if !ValidDBName(name) {
		return "", nil, fmt.Errorf("invalid database name")
	}
	if engine == DBPostgresql {
		return "runuser", []string{"-u", "postgres", "--", "pg_dump", name}, nil
	}
	// --single-transaction dumps InnoDB tables from one consistent snapshot
	// without locking; --quick streams rows instead of buffering the whole
	// table; routines and events cover everything CREATE DATABASE misses.
	args = []string{"--single-transaction", "--quick", "--routines", "--events", name}
	if engine == DBMariadb {
		return "mariadb-dump", args, nil
	}
	return "mysqldump", args, nil
}

// DBRestoreCommand builds the argv for replaying a SQL dump into one
// database: the dump text arrives on stdin. Both clients stop at the first
// failed statement so a broken restore cannot half-apply silently.
func DBRestoreCommand(engine, name string) (bin string, args []string, err error) {
	if !ValidDBEngine(engine) || engine == DBRedis {
		return "", nil, fmt.Errorf("unsupported database engine %q", engine)
	}
	if !ValidDBName(name) {
		return "", nil, fmt.Errorf("invalid database name")
	}
	if engine == DBPostgresql {
		return "runuser", []string{"-u", "postgres", "--", "psql", "-v", "ON_ERROR_STOP=1", "-d", name}, nil
	}
	return dbClient(engine), []string{name}, nil
}

// DBQueryCommand builds the argv for the SQL console: machine-readable output
// with a header row. mysql --batch prints TSV with \t, \n and \\ escaped
// inside values; psql -A -F prints raw TSV (values containing those bytes may
// shift columns, which the console notes).
func DBQueryCommand(engine, name string) (bin string, args []string, err error) {
	if !ValidDBEngine(engine) || engine == DBRedis {
		return "", nil, fmt.Errorf("unsupported database engine %q", engine)
	}
	if !ValidDBName(name) {
		return "", nil, fmt.Errorf("invalid database name")
	}
	if engine == DBPostgresql {
		return "runuser", []string{"-u", "postgres", "--", "psql", "-A", "-F", "\t", "-d", name}, nil
	}
	return dbClient(engine), []string{"--batch", name}, nil
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
// DBCommand/DBCreateCommand/DBUserCommand/DBBackupCommand: runuser is
// resolved directly, otherwise the engine's client (or the dump command's
// own) candidates are tried in preference order. Both ends use this so the
// panel and the helper agree on substitution. available reports whether a
// whitelisted binary exists (without executing it).
func ResolveDBBinary(engine, bin string, available func(string) bool) (string, error) {
	if bin == "runuser" {
		if available("runuser") {
			return "runuser", nil
		}
		return "", fmt.Errorf("required system utility runuser unavailable")
	}
	candidates := DBClientBinaries[engine]
	if dump, ok := dbDumpCandidates[bin]; ok {
		candidates = dump
	}
	for _, candidate := range candidates {
		if available(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no SQL client found for engine %s", engine)
}
