//go:build linux

package helper

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Helper-side execution of database operations. Every argument is rebuilt
// from the shared catalog in databases.go; the request contributes only the
// engine, a validated name and (for user operations) the password, which is
// rendered into SQL and fed over stdin so it never appears in a process list.

// runStdin is run() with stdin content for the child process. Everything else
// (fixed lookup, scrubbed environment, output cap, deadline) is identical.
func runStdin(ctx context.Context, stdin, name string, args ...string) (string, error) {
	p, err := executable(name)
	if err != nil {
		return "", err
	}
	c, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, p, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "SYSTEMD_PAGER=cat", "SYSTEMD_COLORS=0"}
	cmd.WaitDelay = time.Second
	cmd.Stdin = strings.NewReader(stdin)
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

func (s *server) database(req *Request) Response {
	if !s.cfg.AllowDatabases {
		return Response{Error: "database management requires allow_databases = true in [helper]"}
	}
	if !ValidDBAction(req.Action) {
		return Response{Error: "unsupported database action"}
	}
	if !ValidDBEngine(req.DB) {
		return Response{Error: "unsupported database engine"}
	}
	if req.Action == "list" {
		return s.dbList(req.DB)
	}
	if !ValidDBName(req.Name) && !ValidDBUser(req.Name) {
		return Response{Error: "invalid database or user name"}
	}
	bin, args, stdin, err := DBCommand(req.DB, req.Action, req.Name, req.Password)
	if err != nil {
		return Response{Error: err.Error()}
	}
	name, err := resolveDB(req.DB, bin)
	if err != nil {
		return Response{Error: err.Error()}
	}
	out, err := runStdin(context.Background(), stdin, name, args...)
	return respond(out, err)
}

// dbList runs both listing commands and returns a JSON payload of
// {"databases":[...],"users":[...]} in Output; the panel decodes it.
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
	out, err := json.Marshal(map[string][]string{
		"databases": splitLines(dbs),
		"users":     splitLines(users),
	})
	if err != nil {
		return Response{Error: err.Error()}
	}
	return Response{OK: true, Output: string(out)}
}

func splitLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
