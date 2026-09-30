package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingSink captures the HTTP-01 challenge round-trip.
type recordingSink struct {
	puts    [][2]string
	cleared []string
}

func (s *recordingSink) Put(token, keyAuth string) error {
	s.puts = append(s.puts, [2]string{token, keyAuth})
	return nil
}
func (s *recordingSink) Clear(token string) error {
	s.cleared = append(s.cleared, token)
	return nil
}

// mockACME is a minimal RFC 8555 certificate authority: enough protocol for
// the Issue flow (directory, account, order, HTTP-01 authorization,
// finalize, certificate download). JWS signatures are decoded but not
// verified — the client under test does the signing, the mock does not
// authenticate it.
type mockACME struct {
	t          *testing.T
	mu         sync.Mutex
	base       string
	token      string
	authzValid bool
	domain     string
	csrDomain  string
	tosPrompt  bool
	staging    bool
	chainPEM   []byte
	sink       *recordingSink
}

func (m *mockACME) decodePayload(r *http.Request) map[string]any {
	var jws struct {
		Payload string `json:"payload"`
	}
	if err := json.NewDecoder(r.Body).Decode(&jws); err != nil {
		m.t.Fatalf("bad JWS envelope: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(jws.Payload)
	if err != nil {
		m.t.Fatalf("bad payload encoding: %v", err)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			m.t.Fatalf("bad payload JSON: %v", err)
		}
	}
	return out
}

func (m *mockACME) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Replay-Nonce", "nonce-"+time.Now().Format("150405.000000000"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (m *mockACME) handler() http.Handler {
	mux := http.NewServeMux()
	// The directory endpoint sits at the root, exactly how a real CA URL is
	// handed to acme.Client.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			m.t.Logf("mock: unmatched %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		m.write(w, 200, map[string]any{
			"newNonce":   m.base + "/new-nonce",
			"newAccount": m.base + "/new-account",
			"newOrder":   m.base + "/new-order",
			"meta":       map[string]any{"termsOfService": m.base + "/terms"},
		})
	})
	mux.HandleFunc("/new-nonce", func(w http.ResponseWriter, r *http.Request) {
		m.write(w, 200, map[string]any{})
	})
	mux.HandleFunc("/new-account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", m.base+"/acct/1")
		m.write(w, http.StatusCreated, map[string]any{"status": "valid"})
	})
	mux.HandleFunc("/new-order", func(w http.ResponseWriter, r *http.Request) {
		payload := m.decodePayload(r)
		idents, _ := payload["identifiers"].([]any)
		if len(idents) != 1 {
			m.t.Fatal("expected exactly one identifier")
		}
		id := idents[0].(map[string]any)
		m.mu.Lock()
		m.domain, _ = id["value"].(string)
		m.mu.Unlock()
		w.Header().Set("Location", m.base+"/order/1")
		m.write(w, http.StatusCreated, map[string]any{
			"status":         "pending",
			"identifiers":    []map[string]string{{"type": "dns", "value": m.domain}},
			"authorizations": []string{m.base + "/authz/1"},
			"finalize":       m.base + "/finalize/1",
			"expires":        time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		})
	})
	mux.HandleFunc("/authz/1", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		valid := m.authzValid
		m.mu.Unlock()
		status := "pending"
		if valid {
			status = "valid"
		}
		m.write(w, 200, map[string]any{
			"status":     status,
			"identifier": map[string]string{"type": "dns", "value": m.domain},
			"challenges": []map[string]any{{
				"type":   "http-01",
				"token":  m.token,
				"url":    m.base + "/chal/1",
				"status": status,
			}},
		})
	})
	mux.HandleFunc("/chal/1", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.authzValid = true
		m.mu.Unlock()
		m.write(w, 200, map[string]any{
			"type": "http-01", "token": m.token, "url": m.base + "/chal/1", "status": "valid",
		})
	})
	mux.HandleFunc("/finalize/1", func(w http.ResponseWriter, r *http.Request) {
		payload := m.decodePayload(r)
		csrB64, _ := payload["csr"].(string)
		der, err := base64.RawURLEncoding.DecodeString(csrB64)
		if err != nil {
			m.t.Fatalf("bad CSR encoding: %v", err)
		}
		csr, err := x509.ParseCertificateRequest(der)
		if err != nil {
			m.t.Fatalf("bad CSR: %v", err)
		}
		m.mu.Lock()
		m.csrDomain = csr.DNSNames[0]
		m.mu.Unlock()
		w.Header().Set("Location", m.base+"/order/1")
		m.write(w, 200, map[string]any{
			"status":      "valid",
			"certificate": m.base + "/cert/1",
		})
	})
	mux.HandleFunc("/cert/1", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Replay-Nonce", "nonce")
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		w.WriteHeader(200)
		_, _ = w.Write(m.chainPEM)
	})
	return mux
}

func TestIssueAgainstMockACME(t *testing.T) {
	const domain = "blog.example.com"
	token := strings.Repeat("t", 43)
	// Build the certificate the mock CA will hand out.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "Mock ACME Intermediate"},
		DNSNames:     []string{domain},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	chainPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	sink := &recordingSink{}
	mock := &mockACME{t: t, token: token, chainPEM: chainPEM, sink: sink}
	server := httptest.NewServer(mock.handler())
	defer server.Close()
	mock.base = server.URL

	store := NewStore(t.TempDir())
	var lines []string
	issuer := &Issuer{Store: store, Directory: server.URL, Log: func(line string) { lines = append(lines, line) }}
	gotChain, gotKey, err := issuer.Issue(context.Background(), domain, "a@b.co", false, sink)
	if err != nil {
		t.Fatalf("issue: %v\nlog:\n%s", err, strings.Join(lines, "\n"))
	}
	leaf, err := LeafFromPEM(gotChain)
	if err != nil {
		t.Fatalf("issued chain unparseable: %v", err)
	}
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != domain {
		t.Fatalf("wrong leaf SANs: %v", leaf.DNSNames)
	}
	if leaf.Issuer.CommonName != "Mock ACME Intermediate" {
		t.Fatalf("wrong leaf issuer: %q", leaf.Issuer.CommonName)
	}
	if !strings.Contains(string(gotKey), "EC PRIVATE KEY") {
		t.Fatalf("site key PEM missing: %q", string(gotKey[:32]))
	}
	if mock.domain != domain || mock.csrDomain != domain {
		t.Fatalf("mock saw domain %q / CSR %q", mock.domain, mock.csrDomain)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "接受 ACME 服务条款") {
		t.Fatal("terms-of-service prompt was never logged")
	}
	// The HTTP-01 challenge went through the sink: token plus the account
	// thumbprint, and was cleared afterwards.
	if len(sink.puts) != 1 || sink.puts[0][0] != token {
		t.Fatalf("wrong challenge writes: %v", sink.puts)
	}
	auth := sink.puts[0][1]
	if !strings.HasPrefix(auth, token+".") || len(auth) != len(token)+44 {
		t.Fatalf("key authorization malformed: %q", auth)
	}
	thumb, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(auth, token+"."))
	if err != nil || len(thumb) != sha256.Size {
		t.Fatalf("thumbprint is not sha256-sized: %v", err)
	}
	if len(sink.cleared) != 1 || sink.cleared[0] != token {
		t.Fatalf("challenge not cleared: %v", sink.cleared)
	}
}
