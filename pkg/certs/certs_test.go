package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testChain builds a self-signed leaf certificate and returns its PEM chain
// together with the matching site key.
func testChain(t *testing.T, domain string, notAfter time.Time) (chainPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "LightPanel Test CA"},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	chainPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if keyPEM, err = EncodeKeyPEM(key); err != nil {
		t.Fatal(err)
	}
	return chainPEM, keyPEM
}

func TestStoreRoundtrip(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	notAfter := time.Now().Add(60 * 24 * time.Hour)
	chain, key := testChain(t, "blog.example.com", notAfter)
	meta := Meta{
		Domain: "blog.example.com", Email: "a@b.co", Staging: false, ForceHTTPS: true,
		Site: Site{Name: "blog-example-com", Engine: "nginx", Kind: "static", Domain: "blog.example.com", Port: 80, Root: "/var/www/blog"},
	}
	if err := store.Save(meta, chain, key); err != nil {
		t.Fatalf("save: %v", err)
	}
	certPath, keyPath := store.Paths("blog.example.com")
	if certPath != filepath.Join(dir, "certs", "blog.example.com", "fullchain.pem") {
		t.Fatalf("unexpected cert path %s", certPath)
	}
	if fi, err := os.Stat(keyPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("site key must exist with 0600: %v %v", fi, err)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v (%d items)", err, len(items))
	}
	item := items[0]
	if item.Domain != "blog.example.com" || !item.ForceHTTPS || item.Staging {
		t.Fatalf("wrong metadata: %+v", item)
	}
	if item.Issuer != "LightPanel Test CA" {
		t.Fatalf("issuer %q", item.Issuer)
	}
	if item.DaysLeft < 58 || item.DaysLeft > 61 {
		t.Fatalf("days left %d", item.DaysLeft)
	}
	if item.Site.Engine != "nginx" || item.Site.Port != 80 {
		t.Fatalf("site params lost: %+v", item.Site)
	}
	stored, ok, err := store.Meta("blog.example.com")
	if err != nil || !ok || stored.Email != "a@b.co" {
		t.Fatalf("meta: ok=%v err=%v", ok, err)
	}
	// A 60-day certificate is not due for renewal yet.
	if due, err := store.Due("blog.example.com"); err != nil || due {
		t.Fatalf("fresh cert reported due: %v %v", due, err)
	}
	// Missing certificates are always due.
	if due, err := store.Due("other.example.com"); err != nil || !due {
		t.Fatalf("missing cert must be due: %v %v", due, err)
	}
}

func TestStoreDueRenewWindow(t *testing.T) {
	store := NewStore(t.TempDir())
	chain, key := testChain(t, "old.example.com", time.Now().Add(10*24*time.Hour))
	if err := store.Save(Meta{Domain: "old.example.com"}, chain, key); err != nil {
		t.Fatal(err)
	}
	if due, err := store.Due("old.example.com"); err != nil || !due {
		t.Fatalf("cert expiring in 10 days must be due: %v %v", due, err)
	}
}

func TestAccountKeyPersistence(t *testing.T) {
	store := NewStore(t.TempDir())
	key, err := store.AccountKey()
	if err != nil {
		t.Fatalf("create account key: %v", err)
	}
	again, err := store.AccountKey()
	if err != nil {
		t.Fatalf("reload account key: %v", err)
	}
	if key.D.Cmp(again.D) != 0 || key.X.Cmp(again.X) != 0 {
		t.Fatal("account key was regenerated instead of reused")
	}
}

func TestListSkipsGarbage(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := os.MkdirAll(filepath.Join(store.Dir, "certs", "broken.example.com"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "certs", "broken.example.com", "fullchain.pem"), []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	chain, key := testChain(t, "good.example.com", time.Now().Add(24*time.Hour))
	if err := store.Save(Meta{Domain: "good.example.com"}, chain, key); err != nil {
		t.Fatal(err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Domain != "good.example.com" {
		t.Fatalf("garbage entries must be skipped, got %+v", items)
	}
}
