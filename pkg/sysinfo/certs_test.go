package sysinfo

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"lightpanel/pkg/certs"
	"lightpanel/pkg/helper"
)

// fakeIssuer stands in for the ACME round-trip: it writes one challenge
// through the sink (exercising the helper plumbing) and returns a fixed
// self-signed chain.
type fakeIssuer struct {
	token string
}

func (f fakeIssuer) Issue(_ context.Context, domain, _ string, _ bool, sink certs.ChallengeSink) ([]byte, []byte, error) {
	if err := sink.Put(f.token, f.token+".aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		return nil, nil, err
	}
	if err := sink.Clear(f.token); err != nil {
		return nil, nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3),
		Subject:      pkix.Name{CommonName: "Fake CA"},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, err
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := certs.EncodeKeyPEM(key)
	if err != nil {
		return nil, nil, err
	}
	return chain, keyPEM, nil
}

func testSiteManagerForCerts() *SiteManager {
	return &SiteManager{
		Run: func(_ context.Context, _ string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "list-unit-files" {
				return "nginx.service enabled\n", nil
			}
			return "", nil
		},
		RunTimeout: func(context.Context, time.Duration, string, ...string) (string, error) { return "", nil },
	}
}

func waitCertTask(t *testing.T, tasks *TaskManager) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tasks.mu.Lock()
		state := ""
		if len(tasks.tasks) > 0 {
			state = tasks.tasks[0].State
		}
		tasks.mu.Unlock()
		if state == TaskDone || state == TaskError {
			if state == TaskError {
				tasks.mu.Lock()
				errText := tasks.tasks[0].Error
				tasks.mu.Unlock()
				t.Fatalf("issuance task failed: %s", errText)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("issuance task did not finish (state %q)", state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSSLIssueFlow(t *testing.T) {
	previous := PrivilegedCall
	t.Cleanup(func() { PrivilegedCall = previous })
	var mu sync.Mutex
	var requests []helper.Request
	PrivilegedCall = func(_ context.Context, req helper.Request) (string, error) {
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		return "", nil
	}
	site := &managedSite{
		ID: "/etc/nginx/conf.d/blog.conf", Engine: "nginx", Kind: "static",
		Domain: "blog.example.com", Port: 80, Root: "/var/www/blog", SiteName: "blog",
	}
	store := certs.NewStore(t.TempDir())
	cm := NewCertManager(testSiteManagerForCerts(), nil, store)
	cm.loadSite = func(string) (*managedSite, error) { return site, nil }
	cm.newIssuer = func(_ *certs.Store, _ certs.Progress) certIssuer {
		return fakeIssuer{token: strings.Repeat("k", 43)}
	}
	form := url.Values{"op": {"issue"}, "id": {site.ID}, "email": {"a@b.co"}, "force_https": {"true"}}
	r := httptest.NewRequest("POST", "/api/sites/ssl", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	cm.SSL(w, r)
	if w.Code != 200 {
		t.Fatalf("issuance rejected: %d %s", w.Code, w.Body.String())
	}
	waitCertTask(t, cm.TaskCenter())

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 5 {
		t.Fatalf("expected 5 helper requests, got %d: %+v", len(requests), requests)
	}
	// 1: challenge-phase configuration (no SSL yet)
	if a := requests[0]; a.Action != "ssl-apply" || a.SSLOn || a.Site != "blog" || a.Domain != "blog.example.com" {
		t.Fatalf("wrong challenge conf request: %+v", a)
	}
	// 2: challenge file written, 3: removed again by the issuer's cleanup
	if a := requests[1]; a.Action != "challenge-set" || len(a.Token) != 43 || a.Auth != a.Token+"."+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("wrong challenge-set request: %+v", a)
	}
	if a := requests[2]; a.Action != "challenge-clear" || a.Token != requests[1].Token {
		t.Fatalf("wrong challenge-clear request: %+v", a)
	}
	// 4: final HTTPS configuration pointing at the stored certificate files
	final := requests[3]
	if final.Action != "ssl-apply" || !final.SSLOn || !final.ForceHTTPS {
		t.Fatalf("wrong final conf request: %+v", final)
	}
	if !strings.HasPrefix(final.CertFile, store.Dir) || !strings.HasPrefix(final.KeyFile, store.Dir) ||
		!strings.HasSuffix(final.CertFile, "fullchain.pem") || !strings.HasSuffix(final.KeyFile, "privkey.pem") {
		t.Fatalf("cert files outside store: %s %s", final.CertFile, final.KeyFile)
	}
	// 5: janitor clears the challenge directory
	if a := requests[4]; a.Action != "challenge-clear" || a.Token != "" {
		t.Fatalf("wrong janitor request: %+v", a)
	}
	items, err := store.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("store listing: %v (%d)", err, len(items))
	}
	if items[0].ForceHTTPS != true || items[0].Email != "a@b.co" {
		t.Fatalf("wrong stored metadata: %+v", items[0])
	}
}

func TestSSLOffFlow(t *testing.T) {
	previous := PrivilegedCall
	t.Cleanup(func() { PrivilegedCall = previous })
	var requests []helper.Request
	PrivilegedCall = func(_ context.Context, req helper.Request) (string, error) {
		requests = append(requests, req)
		return "", nil
	}
	site := &managedSite{
		ID: "/etc/nginx/conf.d/blog.conf", Engine: "nginx", Kind: "static", SSL: true,
		Domain: "blog.example.com", Port: 80, Root: "/var/www/blog", SiteName: "blog",
	}
	cm := NewCertManager(testSiteManagerForCerts(), nil, certs.NewStore(t.TempDir()))
	cm.loadSite = func(string) (*managedSite, error) { return site, nil }
	form := url.Values{"op": {"off"}, "id": {site.ID}}
	r := httptest.NewRequest("POST", "/api/sites/ssl", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	cm.SSL(w, r)
	if w.Code != 200 {
		t.Fatalf("ssl off rejected: %d %s", w.Code, w.Body.String())
	}
	if len(requests) != 1 || requests[0].Action != "ssl-apply" || requests[0].SSLOn {
		t.Fatalf("wrong ssl-off request: %+v", requests)
	}
}

func TestSSLRejectsUnmanagedSites(t *testing.T) {
	cm := NewCertManager(testSiteManagerForCerts(), nil, certs.NewStore(t.TempDir()))
	// 未被 helper 管理的路径直接拒绝，不会触发任何特权调用。
	PrivilegedCall = func(_ context.Context, _ helper.Request) (string, error) {
		return "", errors.New("must not be called")
	}
	defer func() { PrivilegedCall = nil }()
	for _, id := range []string{"", "/etc/nginx/nginx.conf", "/etc/nginx/conf.d/../../evil.conf", "relative.conf"} {
		form := url.Values{"op": {"issue"}, "id": {id}}
		r := httptest.NewRequest("POST", "/api/sites/ssl", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		cm.SSL(w, r)
		if w.Code != 400 {
			t.Fatalf("id %q accepted: %d %s", id, w.Code, w.Body.String())
		}
	}
}

func TestCertificatesListEndpoint(t *testing.T) {
	store := certs.NewStore(t.TempDir())
	chain, key := testChainForCerts(t, "blog.example.com")
	if err := store.Save(certs.Meta{Domain: "blog.example.com", Email: "a@b.co", ForceHTTPS: true}, chain, key); err != nil {
		t.Fatal(err)
	}
	cm := NewCertManager(testSiteManagerForCerts(), nil, store)
	w := httptest.NewRecorder()
	cm.List(w, httptest.NewRequest("GET", "/api/sites/certs", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"domain":"blog.example.com"`) || !strings.Contains(w.Body.String(), "Fake CA") {
		t.Fatalf("cert listing failed: %d %s", w.Code, w.Body.String())
	}
}

func testChainForCerts(t *testing.T, domain string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(4),
		Subject:      pkix.Name{CommonName: "Fake CA"},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(60 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := certs.EncodeKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	return chain, keyPEM
}
