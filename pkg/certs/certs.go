// Package certs implements the panel's built-in ACME client and the on-disk
// certificate store. Certificate issuance talks to Let's Encrypt directly
// over the ACME protocol (golang.org/x/crypto/acme), so no external tool
// such as certbot is needed; the HTTP-01 challenge itself is answered by the
// site's web server from files the panel plants through the privileged
// helper (see pkg/helper ChallengeDir).
//
// State layout under the store directory:
//
//	account.key                    ACME account key (ECDSA P-256, 0600)
//	certs/<domain>/fullchain.pem   leaf + intermediates, 0644
//	certs/<domain>/privkey.pem     site key, 0600
//	certs/<domain>/meta.json       issuance metadata for renewal / listing
package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Let's Encrypt ACME directories. Staging signs with an untrusted root and
// is exempt from the strict production rate limits.
const (
	ProductionDirectory = "https://acme-v02.api.letsencrypt.org/directory"
	StagingDirectory    = "https://acme-staging-v02.api.letsencrypt.org/directory"
)

// DirectoryURL maps the staging flag to an ACME directory endpoint.
func DirectoryURL(staging bool) string {
	if staging {
		return StagingDirectory
	}
	return ProductionDirectory
}

// Store is the on-disk certificate/account state rooted at one directory.
type Store struct {
	Dir string
}

// Site captures the site parameters an SSL configuration was applied with,
// so renewals can re-apply the same settings without re-parsing the engine.
type Site struct {
	Name        string `json:"name"`
	Engine      string `json:"engine"`
	Kind        string `json:"kind"`
	Domain      string `json:"domain"`
	Port        int    `json:"port"`
	Root        string `json:"root,omitempty"`
	ProxyTarget string `json:"proxy_target,omitempty"`
}

// Meta is the per-certificate metadata persisted next to the PEM files.
type Meta struct {
	Domain     string    `json:"domain"`
	Email      string    `json:"email,omitempty"`
	Staging    bool      `json:"staging"`
	ForceHTTPS bool      `json:"force_https"`
	IssuedAt   time.Time `json:"issued_at"`
	Site       Site      `json:"site"`
}

// Issued is one stored certificate as reported by List: parsed from the
// PEM chain, enriched with the persisted metadata.
type Issued struct {
	Domain     string    `json:"domain"`
	Email      string    `json:"email,omitempty"`
	Staging    bool      `json:"staging"`
	ForceHTTPS bool      `json:"force_https"`
	NotBefore  time.Time `json:"not_before"`
	NotAfter   time.Time `json:"not_after"`
	DaysLeft   int       `json:"days_left"`
	Issuer     string    `json:"issuer"`
	Site       Site      `json:"site"`
}

// NewStore returns a store rooted at dir (created on demand).
func NewStore(dir string) *Store { return &Store{Dir: dir} }

// ResolveDir picks the persistent state directory for ACME data: the shared
// /var/lib location when the current user can write it (root panel, or a
// pre-created panel-owned directory from the install script), otherwise the
// user's XDG state directory.
func ResolveDir() string {
	dir := "/var/lib/lightpanel/acme"
	if writable(dir) {
		return dir
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "lightpanel", "acme")
	}
	return dir
}

func writable(dir string) bool {
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

func (s *Store) certDir(domain string) string {
	return filepath.Join(s.Dir, "certs", domain)
}

// Paths returns the PEM file locations handed to the web server config.
func (s *Store) Paths(domain string) (cert, key string) {
	return filepath.Join(s.certDir(domain), "fullchain.pem"), filepath.Join(s.certDir(domain), "privkey.pem")
}

// AccountKey loads the ACME account key, creating a P-256 key on first use.
func (s *Store) AccountKey() (*ecdsa.PrivateKey, error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Dir, "account.key")
	if data, err := os.ReadFile(path); err == nil {
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, errors.New("account key file is not PEM")
		}
		key, err := x509.ParseECPrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse account key: %w", err)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Save persists one issued certificate (leaf first) and its metadata.
func (s *Store) Save(meta Meta, chainPEM, keyPEM []byte) error {
	if meta.Domain == "" {
		return errors.New("domain is required")
	}
	dir := s.certDir(meta.Domain)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	meta.IssuedAt = time.Now()
	blob, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), blob, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), chainPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "privkey.pem"), keyPEM, 0o600); err != nil {
		return err
	}
	return nil
}

// Meta loads the stored metadata for one domain.
func (s *Store) Meta(domain string) (Meta, bool, error) {
	blob, err := os.ReadFile(filepath.Join(s.certDir(domain), "meta.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Meta{}, false, nil
	}
	if err != nil {
		return Meta{}, false, err
	}
	var meta Meta
	if err := json.Unmarshal(blob, &meta); err != nil {
		return Meta{}, false, err
	}
	return meta, true, nil
}

// List reports all stored certificates, earliest expiry first. Certificates
// whose PEM cannot be parsed are skipped rather than failing the listing.
func (s *Store) List() ([]Issued, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "certs"))
	if errors.Is(err, os.ErrNotExist) {
		return []Issued{}, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Issued
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		item, err := s.read(entry.Name())
		if err != nil {
			continue
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].NotAfter.Before(items[j].NotAfter) })
	return items, nil
}

func (s *Store) read(domain string) (Issued, error) {
	certPath, _ := s.Paths(domain)
	blob, err := os.ReadFile(certPath)
	if err != nil {
		return Issued{}, err
	}
	leaf, err := LeafFromPEM(blob)
	if err != nil {
		return Issued{}, err
	}
	meta, ok, err := s.Meta(domain)
	if err != nil {
		return Issued{}, err
	}
	if !ok {
		meta.Domain = domain
	}
	return Issued{
		Domain:     domain,
		Email:      meta.Email,
		Staging:    meta.Staging,
		ForceHTTPS: meta.ForceHTTPS,
		NotBefore:  leaf.NotBefore,
		NotAfter:   leaf.NotAfter,
		DaysLeft:   int(time.Until(leaf.NotAfter).Hours() / 24),
		Issuer:     issuerName(leaf),
		Site:       meta.Site,
	}, nil
}

// Due reports whether the certificate needs renewal: missing, or expiring
// within the renew window (30 days, matching certbot's default).
func (s *Store) Due(domain string) (bool, error) {
	certPath, _ := s.Paths(domain)
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	item, err := s.read(domain)
	if err != nil {
		return true, nil // unreadable cert: force a fresh issuance
	}
	return time.Until(item.NotAfter) < 30*24*time.Hour, nil
}

// LeafFromPEM parses the first PEM certificate in blob.
func LeafFromPEM(blob []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(blob)
	if block == nil {
		return nil, errors.New("no PEM certificate found")
	}
	return x509.ParseCertificate(block.Bytes)
}

func issuerName(cert *x509.Certificate) string {
	name := cert.Issuer.CommonName
	if name == "" {
		name = strings.Join(cert.Issuer.Organization, " ")
	}
	if name == "" {
		name = "未知颁发者"
	}
	return name
}

// EncodeKeyPEM serializes an ECDSA site key in the traditional SEC1 form.
func EncodeKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// GenerateSiteKey creates a fresh P-256 key for one certificate.
func GenerateSiteKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

var _ crypto.Signer = (*ecdsa.PrivateKey)(nil)
