package gcp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	tokenAudience      = "https://oauth2.googleapis.com/token"
	defaultTokenURL    = "https://oauth2.googleapis.com/token"
	defaultMetadataURL = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
	metadataFlavor     = "Google"
	jwtGrantType       = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	tokenRefreshSkew   = 60 * time.Second
)

type serviceAccount struct {
	clientEmail  string
	privateKey   *rsa.PrivateKey
	privateKeyID string
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

type tokenSource struct {
	cfg Config
	sa  *serviceAccount

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func parseServiceAccountJSON(raw []byte) (serviceAccount, error) {
	var doc struct {
		ClientEmail  string `json:"client_email"`
		PrivateKey   string `json:"private_key"`
		PrivateKeyID string `json:"private_key_id"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return serviceAccount{}, fmt.Errorf("secret/gcp: parse service account json: %w", err)
	}
	if doc.ClientEmail == "" || doc.PrivateKey == "" {
		return serviceAccount{}, fmt.Errorf("secret/gcp: service account json missing client_email or private_key")
	}
	key, err := parseRSAPrivateKeyPEM([]byte(doc.PrivateKey))
	if err != nil {
		return serviceAccount{}, err
	}
	return serviceAccount{
		clientEmail:  doc.ClientEmail,
		privateKey:   key,
		privateKeyID: doc.PrivateKeyID,
	}, nil
}

func parseRSAPrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("secret/gcp: no PEM block in private_key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("secret/gcp: parse private key: %w", err)
		}
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("secret/gcp: private_key is not RSA")
	}
	return rsaKey, nil
}

func buildJWTAssertion(sa serviceAccount, now time.Time) (string, error) {
	header := map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	}
	if sa.privateKeyID != "" {
		header["kid"] = sa.privateKeyID
	}

	iat := now.Unix()
	claims := map[string]any{
		"iss":   sa.clientEmail,
		"scope": cloudPlatformScope,
		"aud":   tokenAudience,
		"iat":   iat,
		"exp":   iat + 3600,
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", fmt.Errorf("secret/gcp: marshal jwt header: %w", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("secret/gcp: marshal jwt claims: %w", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, sa.privateKey, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("secret/gcp: sign jwt: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (ts *tokenSource) accessToken(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	now := time.Now
	if ts.cfg.Now != nil {
		now = ts.cfg.Now
	}
	if ts.token != "" && now().Before(ts.expiry.Add(-tokenRefreshSkew)) {
		return ts.token, nil
	}

	token, expiry, err := ts.fetchToken(ctx, now())
	if err != nil {
		return "", err
	}
	ts.token = token
	ts.expiry = expiry
	return token, nil
}

func (ts *tokenSource) fetchToken(ctx context.Context, now time.Time) (string, time.Time, error) {
	if ts.cfg.UseMetadataServer {
		return ts.fetchMetadataToken(ctx)
	}
	if ts.sa == nil {
		return "", time.Time{}, fmt.Errorf("secret/gcp: service account credentials are not configured")
	}
	assertion, err := buildJWTAssertion(*ts.sa, now)
	if err != nil {
		return "", time.Time{}, err
	}
	return ts.exchangeAssertion(ctx, assertion)
}

func (ts *tokenSource) fetchMetadataToken(ctx context.Context) (string, time.Time, error) {
	endpoint := defaultMetadataURL
	if ts.cfg.MetadataEndpoint != "" {
		endpoint = ts.cfg.MetadataEndpoint
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("secret/gcp: build metadata request: %w", err)
	}
	req.Header.Set("Metadata-Flavor", metadataFlavor)

	client := ts.cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("secret/gcp: metadata token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("secret/gcp: read metadata response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("secret/gcp: metadata server returned HTTP %d", resp.StatusCode)
	}

	var out tokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("secret/gcp: decode metadata token: %w", err)
	}
	if out.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("secret/gcp: metadata response missing access_token")
	}
	expiresIn := out.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return out.AccessToken, time.Now().Add(time.Duration(expiresIn) * time.Second), nil
}

func (ts *tokenSource) exchangeAssertion(ctx context.Context, assertion string) (string, time.Time, error) {
	endpoint := defaultTokenURL
	if ts.cfg.TokenEndpoint != "" {
		endpoint = ts.cfg.TokenEndpoint
	}

	form := url.Values{}
	form.Set("grant_type", jwtGrantType)
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("secret/gcp: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := ts.cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("secret/gcp: token request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("secret/gcp: read token response: %w", err)
	}

	var out tokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", time.Time{}, fmt.Errorf("secret/gcp: decode token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := out.ErrorDesc
		if msg == "" {
			msg = out.Error
		}
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		return "", time.Time{}, fmt.Errorf("secret/gcp: token exchange failed: %s (HTTP %d)", msg, resp.StatusCode)
	}
	if out.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("secret/gcp: token response missing access_token")
	}
	expiresIn := out.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return out.AccessToken, time.Now().Add(time.Duration(expiresIn) * time.Second), nil
}
