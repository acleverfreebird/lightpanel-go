package sysinfo

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Credential notes for databases and users created through the panel. Like
// BT Panel's database list, the panel remembers the account it provisioned
// (user, password, access host) so the list can show and copy it again —
// without it a generated password would be unrepeatable. The file holds only
// what the panel itself set or changed and is written 0600 into the panel's
// state directory; databases created outside the panel simply have no note.

const dbCredentialFileVersion = 1

type dbCredential struct {
	User     string    `json:"user,omitempty"`
	Password string    `json:"password,omitempty"`
	Host     string    `json:"host,omitempty"`
	Charset  string    `json:"charset,omitempty"`
	Updated  time.Time `json:"updated"`
}

type dbCredentialFile struct {
	Version int                     `json:"version"`
	Entries map[string]dbCredential `json:"entries"`
}

// dbCredentialStore is a tiny JSON-file-backed key/value store guarded by a
// mutex (handlers may run concurrently). Keys are kind/engine/name with kind
// "db" or "user".
type dbCredentialStore struct {
	mu   sync.Mutex
	path string
}

func newDBCredentialStore() *dbCredentialStore {
	return &dbCredentialStore{path: resolveDBCredentialPath()}
}

func dbCredentialKey(kind, engine, name string) string {
	return kind + "/" + engine + "/" + name
}

// resolveDBCredentialPath mirrors the ACME store's resolution: the shared
// /var/lib location when the current user can use it (root panel, or the
// pre-created panel-owned directory from the install script), otherwise the
// user's XDG state directory.
func resolveDBCredentialPath() string {
	dir := "/var/lib/lightpanel/db"
	if writableDir(dir) {
		return filepath.Join(dir, "credentials.json")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "lightpanel", "db", "credentials.json")
	}
	return filepath.Join(dir, "credentials.json")
}

func writableDir(dir string) bool {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false
	}
	probe, err := os.CreateTemp(dir, ".lightpanel-probe-*")
	if err != nil {
		return false
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)
	return true
}

func (s *dbCredentialStore) load() (map[string]dbCredential, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]dbCredential{}, nil
		}
		return nil, err
	}
	var file dbCredentialFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, err
	}
	if file.Version != dbCredentialFileVersion || file.Entries == nil {
		return map[string]dbCredential{}, nil
	}
	return file.Entries, nil
}

// save writes atomically: temporary file in the same directory, 0600, rename.
func (s *dbCredentialStore) save(entries map[string]dbCredential) error {
	data, err := json.MarshalIndent(dbCredentialFile{Version: dbCredentialFileVersion, Entries: entries}, "", " ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path)
}

// get returns the note for key, if it carries anything worth showing.
func (s *dbCredentialStore) get(key string) (dbCredential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return dbCredential{}, false
	}
	cred, ok := entries[key]
	if !ok || (cred.User == "" && cred.Password == "") {
		return dbCredential{}, false
	}
	return cred, true
}

func (s *dbCredentialStore) upsert(key string, cred dbCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		entries = map[string]dbCredential{}
	}
	cred.Updated = time.Now()
	entries[key] = cred
	return s.save(entries)
}

func (s *dbCredentialStore) delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		return err
	}
	if _, ok := entries[key]; !ok {
		return nil
	}
	delete(entries, key)
	return s.save(entries)
}

// rotatePassword records a new password for a user and refreshes every
// database note in the same engine whose account matches, so the databases
// list never shows a stale password after a change.
func (s *dbCredentialStore) rotatePassword(engine, user, password string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.load()
	if err != nil {
		entries = map[string]dbCredential{}
	}
	now := time.Now()
	entries[dbCredentialKey("user", engine, user)] = dbCredential{Password: password, Updated: now}
	for key, cred := range entries {
		parts := strings.SplitN(key, "/", 3)
		if len(parts) == 3 && parts[0] == "db" && parts[1] == engine && cred.User == user {
			cred.Password = password
			cred.Updated = now
			entries[key] = cred
		}
	}
	return s.save(entries)
}
