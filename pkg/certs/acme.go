package certs

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/crypto/acme"
)

// ChallengeSink delivers HTTP-01 key authorizations to whatever serves
// /.well-known/acme-challenge/ for the domain. Panel-side implementations
// route through the privileged helper (challenge-set/challenge-clear) or
// write to the challenge directory directly in root mode.
type ChallengeSink interface {
	Put(token, keyAuth string) error
	Clear(token string) error
}

// Progress receives human-readable issuance progress lines (task log).
type Progress func(string)

// Issuer performs ACME certificate issuance against Let's Encrypt with the
// HTTP-01 challenge type.
type Issuer struct {
	Store *Store
	// HTTPClient defaults to a 30s-timeout client.
	HTTPClient *http.Client
	// Directory overrides the ACME directory endpoint (tests and
	// self-hosted CAs); empty selects Let's Encrypt by the staging flag.
	Directory string
	Log       Progress
}

func (is *Issuer) log(format string, args ...any) {
	if is.Log != nil {
		is.Log(fmt.Sprintf(format, args...))
	}
}

// Issue runs one full issuance round for domain: account bootstrap, order,
// HTTP-01 authorizations, finalize and CSR. It returns the PEM certificate
// chain (leaf first) and the PEM site key. sink.Put/Clear bracket every
// challenge; Clear runs even when authorization fails.
func (is *Issuer) Issue(ctx context.Context, domain, email string, staging bool, sink ChallengeSink) (chainPEM, keyPEM []byte, err error) {
	key, err := is.Store.AccountKey()
	if err != nil {
		return nil, nil, fmt.Errorf("load ACME account key: %w", err)
	}
	client := &acme.Client{
		Key:          key,
		HTTPClient:   is.httpClient(),
		DirectoryURL: is.Directory,
	}
	if client.DirectoryURL == "" {
		client.DirectoryURL = DirectoryURL(staging)
	}
	if err := is.ensureAccount(ctx, client, email); err != nil {
		return nil, nil, err
	}
	is.log("ACME 账号就绪（%s）", directoryLabel(staging))
	is.log("正在创建订单：%s", domain)
	order, err := client.AuthorizeOrder(ctx, []acme.AuthzID{{Type: "dns", Value: domain}})
	if err != nil {
		return nil, nil, fmt.Errorf("create order: %w", err)
	}
	for _, authzURL := range order.AuthzURLs {
		if authzURL == "" {
			continue
		}
		if err := is.authorize(ctx, client, domain, authzURL, sink); err != nil {
			return nil, nil, err
		}
	}
	is.log("域名验证通过，正在生成证书请求…")
	siteKey, err := GenerateSiteKey()
	if err != nil {
		return nil, nil, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: domain},
		DNSNames: []string{domain},
	}, siteKey)
	if err != nil {
		return nil, nil, err
	}
	is.log("正在等待 CA 签发证书…")
	chainDER, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csrDER, true)
	if err != nil {
		return nil, nil, fmt.Errorf("finalize order: %w", err)
	}
	for _, der := range chainDER {
		chainPEM = append(chainPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	leaf, err := LeafFromPEM(chainPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("parse issued certificate: %w", err)
	}
	if leaf.NotAfter.Before(time.Now()) {
		return nil, nil, errors.New("CA returned an already-expired certificate")
	}
	if keyPEM, err = EncodeKeyPEM(siteKey); err != nil {
		return nil, nil, err
	}
	is.log("证书已签发：有效期至 %s（%s）", leaf.NotAfter.Local().Format("2006-01-02 15:04"), issuerLabel(staging))
	return chainPEM, keyPEM, nil
}

func (is *Issuer) httpClient() *http.Client {
	if is.HTTPClient != nil {
		return is.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// ensureAccount registers the ACME account once per key. RFC 8555 CAs
// answer an existing key with 200 OK, which this package surfaces as
// ErrAccountAlreadyExists; either way the client is ready to sign requests
// afterwards.
func (is *Issuer) ensureAccount(ctx context.Context, client *acme.Client, email string) error {
	reg := &acme.Account{}
	if email != "" {
		reg.Contact = []string{"mailto:" + email}
	}
	_, err := client.Register(ctx, reg, func(tos string) bool {
		is.log("接受 ACME 服务条款：%s", tos)
		return true
	})
	if err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return fmt.Errorf("register ACME account: %w", err)
	}
	return nil
}

// authorize solves one HTTP-01 authorization: plant the challenge file,
// accept the challenge and wait for validation.
func (is *Issuer) authorize(ctx context.Context, client *acme.Client, domain, authzURL string, sink ChallengeSink) error {
	authz, err := client.GetAuthorization(ctx, authzURL)
	if err != nil {
		return fmt.Errorf("fetch authorization for %s: %w", domain, err)
	}
	if authz.Status == acme.StatusValid {
		is.log("%s：授权已有效，跳过验证", domain)
		return nil
	}
	var challenge *acme.Challenge
	for _, ch := range authz.Challenges {
		if ch.Type == "http-01" {
			challenge = ch
			break
		}
	}
	if challenge == nil {
		return fmt.Errorf("CA 未为 %s 提供 HTTP-01 验证方式", domain)
	}
	keyAuth, err := client.HTTP01ChallengeResponse(challenge.Token)
	if err != nil {
		return err
	}
	if err := sink.Put(challenge.Token, keyAuth); err != nil {
		return fmt.Errorf("write challenge file: %w", err)
	}
	defer func() {
		if err := sink.Clear(challenge.Token); err != nil {
			is.log("清理挑战文件失败（不影响结果）：%v", err)
		}
	}()
	is.log("正在验证域名 %s（HTTP-01，路径 /.well-known/acme-challenge/%s…）", domain, shortToken(challenge.Token))
	if _, err := client.Accept(ctx, challenge); err != nil {
		return fmt.Errorf("accept challenge: %w", err)
	}
	if _, err := client.WaitAuthorization(ctx, authz.URI); err != nil {
		return authorizationError(domain, err)
	}
	return nil
}

// authorizationError annotates the most common validation failures with the
// fix the operator needs, before the raw CA detail.
func authorizationError(domain string, err error) error {
	detail := ""
	var acmeErr *acme.Error
	if errors.As(err, &acmeErr) {
		detail = acmeErr.Detail
		if detail == "" {
			detail = acmeErr.ProblemType
		}
	}
	return fmt.Errorf("域名 %s 验证失败：%s。常见原因：域名未解析到本服务器、80 端口未放行或被占用、站点配置未生效。CA 返回：%w", domain, detail, err)
}

func shortToken(token string) string {
	if len(token) > 8 {
		return token[:8] + "…"
	}
	return token
}

func directoryLabel(staging bool) string {
	if staging {
		return "Let's Encrypt 测试环境"
	}
	return "Let's Encrypt 生产环境"
}

func issuerLabel(staging bool) string {
	if staging {
		return "Let's Encrypt Staging"
	}
	return "Let's Encrypt"
}
