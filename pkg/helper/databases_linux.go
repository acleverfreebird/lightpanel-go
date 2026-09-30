//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Helper-side execution of database operations. Every argument is rebuilt
// from the shared catalog in databases.go; the request contributes only the
// engine, a validated name and (for user operations) the password, which is
// rendered into SQL and fed over stdin so it never appears in a process list.
// Backups live in the fixed DBBackupDir: dumps stream stdout into a
// gzip-wrapped file (never buffered — databases exceed any output cap),
// restores stream the gunzipped file back into the client's stdin.

const (
	// A large database can take many minutes to dump or restore; the helper
	// connection deadline and the child process timeout grant that, and the
	// panel runs these two actions inside task-center jobs so no HTTP request
	// waits on them.
	dbBackupConnDeadline = 30 * time.Minute
	dbBackupExecTimeout  = 29 * time.Minute
	// The SQL console is an interactive request (65s browser cap upstream).
	dbQueryExecTimeout = 60 * time.Second
)

// runWithStdin is run() with a stdin reader and an explicit timeout.
func runWithStdin(ctx context.Context, timeout time.Duration, stdin io.Reader, name string, args ...string) (string, error) {
	p, err := executable(name)
	if err != nil {
		return "", err
	}
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(c, p, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "SYSTEMD_PAGER=cat", "SYSTEMD_COLORS=0"}
	cmd.WaitDelay = time.Second
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var b commandBuffer
	cmd.Stdout, cmd.Stderr = &b, &b
	err = cmd.Run()
	if c.Err() != nil {
		return b.String(), c.Err()
	}
	if b.truncated {
		return b.String(), fmt.Errorf("command output exceeded 1 MiB")
	}
	return b.String(), err
}

// runStdin is runWithStdin with the standard deadline and string stdin.
func runStdin(ctx context.Context, stdin, name string, args ...string) (string, error) {
	return runWithStdin(ctx, execTimeout, strings.NewReader(stdin), name, args...)
}

// runDumpToFile streams a command's stdout into out (the backup file) through
// a gzip wrapper, capturing stderr for the error message. The gzip stream is
// always closed so the file stays a valid archive even on failure.
func runDumpToFile(ctx context.Context, timeout time.Duration, out io.Writer, name string, args ...string) (string, error) {
	p, err := executable(name)
	if err != nil {
		return "", err
	}
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(c, p, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "SYSTEMD_PAGER=cat", "SYSTEMD_COLORS=0"}
	cmd.WaitDelay = time.Second
	gzw := DumpGzipWriter(out)
	cmd.Stdout = gzw
	var b commandBuffer
	cmd.Stderr = &b
	runErr := cmd.Run()
	if cerr := gzw.Close(); cerr != nil && runErr == nil {
		runErr = cerr
	}
	if c.Err() != nil {
		return b.String(), c.Err()
	}
	return b.String(), runErr
}

// dbAvailable reports whether a binary can be resolved by the helper's fixed
// lookup, without executing it.
func dbAvailable(name string) bool {
	_, err := executable(name)
	return err == nil
}

// resolveDB resolves the command's first-choice binary to an actual candidate.
func resolveDB(engine, bin string) (string, error) {
	return ResolveDBBinary(engine, bin, dbAvailable)
}

func (s *server) database(uid int, req *Request) Response {
	if !s.cfg.AllowDatabases {
		return Response{Error: "database management requires allow_databases = true in [helper]"}
	}
	if !ValidDBAction(req.Action) {
		return Response{Error: "unsupported database action"}
	}
	if !ValidDBEngine(req.DB) {
		return Response{Error: "unsupported database engine"}
	}
	switch req.Action {
	case "list":
		return s.dbList(req.DB)
	case "backup":
		return s.dbBackup(uid, req.DB, req.Name)
	case "backup-list":
		return s.dbBackupList(req.DB, req.Name)
	case "backup-restore":
		return s.dbBackupRestore(req.DB, req.Name, req.File)
	case "backup-delete":
		return s.dbBackupDelete(req.DB, req.Name, req.File)
	case "query":
		return s.dbQuery(req.DB, req.Name, req.SQL)
	}
	if !ValidDBName(req.Name) && !ValidDBUser(req.Name) {
		return Response{Error: "invalid database or user name"}
	}
	var bin, stdin string
	var args []string
	var err error
	if req.Action == "create-db" {
		bin, args, stdin, err = DBCreateCommand(req.DB, req.Name, DBCreateParams{
			User: req.User, Password: req.Password, Charset: req.Charset, Host: req.Host,
		})
	} else {
		bin, args, stdin, err = DBCommand(req.DB, req.Action, req.Name, req.Password)
	}
	if err != nil {
		return Response{Error: err.Error()}
	}
	name, err := resolveDB(req.DB, bin)
	if err != nil {
		return Response{Error: err.Error()}
	}
	out, err := runWithStdin(context.Background(), execTimeout, strings.NewReader(stdin), name, args...)
	return respond(out, err)
}

// dbList runs both listing commands and returns a JSON payload
// (DBListPayload) in Output; the panel decodes it.
func (s *server) dbList(engine string) Response {
	userBin, userArgs, err := DBUserCommand(engine)
	if err != nil {
		return Response{Error: err.Error()}
	}
	userName, err := resolveDB(engine, userBin)
	if err != nil {
		return Response{Error: err.Error()}
	}
	dbBin, dbArgs, _, err := DBCommand(engine, "list", "", "")
	if err != nil {
		return Response{Error: err.Error()}
	}
	dbName, err := resolveDB(engine, dbBin)
	if err != nil {
		return Response{Error: err.Error()}
	}
	users, err := runStdin(context.Background(), "", userName, userArgs...)
	if err != nil {
		return Response{Output: TrimOutput(users), Error: err.Error()}
	}
	dbs, err := runStdin(context.Background(), "", dbName, dbArgs...)
	if err != nil {
		return Response{Output: TrimOutput(dbs), Error: err.Error()}
	}
	out, err := json.Marshal(DBListPayload{
		Databases: ParseDBList(splitLines(dbs)),
		Users:     splitLines(users),
	})
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Output: string(out)}
}

// dbBackup dumps one database into DBBackupDir under its managed name. The
// dump lands in a temporary file first and is renamed only after a clean
// exit, so a failed run never leaves a truncated archive behind a valid
// name. The file is 0600 and chowned to the panel user (when it is not root
// itself) so the unprivileged panel can stream downloads; the directory is
// 0711 — traversable, never listable — for exactly that purpose.
func (s *server) dbBackup(uid int, engine, name string) Response {
	if !ValidDBName(name) {
		return Response{Error: "invalid database name"}
	}
	bin, args, err := DBBackupCommand(engine, name)
	if err != nil {
		return Response{Error: err.Error()}
	}
	cmdBin, err := resolveDB(engine, bin)
	if err != nil {
		return Response{Error: err.Error()}
	}
	file, err := BackupFileName(engine, name, time.Now())
	if err != nil {
		return Response{Error: err.Error()}
	}
	if err := os.MkdirAll(DBBackupDir, 0o711); err != nil {
		return Response{Error: "cannot create backup directory: " + err.Error()}
	}
	tmp, err := os.CreateTemp(DBBackupDir, ".lightpanel-dbtmp-*")
	if err != nil {
		return Response{Error: "cannot create backup file: " + err.Error()}
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if out, err := runDumpToFile(context.Background(), dbBackupExecTimeout, tmp, cmdBin, args...); err != nil {
		return Response{Output: TrimOutput(out), Error: "dump failed: " + err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return Response{Error: "close backup file: " + err.Error()}
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return Response{Error: "chmod backup file: " + err.Error()}
	}
	if uid != 0 {
		_ = os.Chown(tmpName, uid, -1)
	}
	if err := os.Rename(tmpName, filepath.Join(DBBackupDir, file)); err != nil {
		return Response{Error: "rename backup file: " + err.Error()}
	}
	cleanup = false
	return Response{OK: true, Output: file}
}

// dbBackupEntry is one row of the backup listing.
type dbBackupEntry struct {
	File     string `json:"file"`
	Size     int64  `json:"size"`
	Modified int64  `json:"modified"`
}

// dbBackupList returns the managed backups of one database, newest first.
func (s *server) dbBackupList(engine, name string) Response {
	if !ValidDBName(name) {
		return Response{Error: "invalid database name"}
	}
	entries, err := os.ReadDir(DBBackupDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Response{OK: true, Output: "[]"}
		}
		return Response{Error: err.Error()}
	}
	list := make([]dbBackupEntry, 0)
	for _, entry := range entries {
		if !ValidBackupFile(engine, name, entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		list = append(list, dbBackupEntry{File: entry.Name(), Size: info.Size(), Modified: info.ModTime().Unix()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Modified > list[j].Modified })
	out, err := json.Marshal(list)
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Output: string(out)}
}

// openBackup validates the file name against the engine/database pair and
// opens the regular file inside DBBackupDir. Everything else in the backup
// actions builds on this: a managed name can never escape the directory.
func openBackup(engine, name, file string) (*os.File, error) {
	if !ValidDBName(name) {
		return nil, errors.New("invalid database name")
	}
	if !ValidBackupFile(engine, name, file) {
		return nil, errors.New("unknown backup file")
	}
	path := filepath.Join(DBBackupDir, file)
	st, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("backup file unavailable")
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("backup file unavailable")
	}
	return os.Open(path)
}

// dbBackupRestore streams the gunzipped backup back into the database.
func (s *server) dbBackupRestore(engine, name, file string) Response {
	f, err := openBackup(engine, name, file)
	if err != nil {
		return Response{Error: err.Error()}
	}
	defer f.Close()
	zr, err := DumpGzipReader(f)
	if err != nil {
		return Response{Error: "backup file is not a gzip stream"}
	}
	defer zr.Close()
	bin, args, err := DBRestoreCommand(engine, name)
	if err != nil {
		return Response{Error: err.Error()}
	}
	cmdBin, err := resolveDB(engine, bin)
	if err != nil {
		return Response{Error: err.Error()}
	}
	out, err := runWithStdin(context.Background(), dbBackupExecTimeout, zr, cmdBin, args...)
	return respond(out, err)
}

// dbBackupDelete removes one managed backup file.
func (s *server) dbBackupDelete(engine, name, file string) Response {
	f, err := openBackup(engine, name, file)
	if err != nil {
		return Response{Error: err.Error()}
	}
	path := f.Name()
	f.Close()
	if err := os.Remove(path); err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true}
}

// dbQuery runs one SQL batch against one database and returns the raw client
// output (TSV with a header row).
func (s *server) dbQuery(engine, name, sql string) Response {
	if !ValidDBName(name) {
		return Response{Error: "invalid database name"}
	}
	if !ValidDBSQL(sql) {
		return Response{Error: "SQL is empty or oversized"}
	}
	bin, args, err := DBQueryCommand(engine, name)
	if err != nil {
		return Response{Error: err.Error()}
	}
	cmdBin, err := resolveDB(engine, bin)
	if err != nil {
		return Response{Error: err.Error()}
	}
	out, err := runWithStdin(context.Background(), dbQueryExecTimeout, strings.NewReader(sql), cmdBin, args...)
	return respond(out, err)
}

func splitLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
