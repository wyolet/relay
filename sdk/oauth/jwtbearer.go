package oauth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

// maxTokenWait bounds one token-endpoint call when the caller's context has no sooner deadline.
const maxTokenWait = 15 * time.Second

// JWTBearer is a service-account credential exchanged for access tokens with
// the JWT-bearer grant (RFC 7523): a signed assertion is posted to TokenURL.
// It holds key material; never log or return it.
type JWTBearer struct {
	Issuer       string   `json:"issuer"`
	Subject      string   `json:"subject,omitempty"`
	Audience     string   `json:"audience,omitempty"` // empty = TokenURL
	PrivateKey   string   `json:"privateKey"`         // PEM, RSA PKCS#8 or PKCS#1
	PrivateKeyID string   `json:"privateKeyId,omitempty"`
	TokenURL     string   `json:"tokenUrl"`
	Scopes       []string `json:"scopes,omitempty"`
}

// Validate checks the required fields, that TokenURL is https, that
// PrivateKey parses as an RSA key, and that at least one scope is set.
func (g JWTBearer) Validate() error {
	switch {
	case g.Issuer == "":
		return errors.New("oauth: jwtBearer.issuer is required")
	case g.PrivateKey == "":
		return errors.New("oauth: jwtBearer.privateKey is required")
	case g.TokenURL == "":
		return errors.New("oauth: jwtBearer.tokenUrl is required")
	case len(g.Scopes) == 0 || slices.Contains(g.Scopes, ""):
		return errors.New("oauth: jwtBearer.scopes needs at least one non-empty scope")
	}
	u, err := url.Parse(g.TokenURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("oauth: jwtBearer.tokenUrl must be an https URL")
	}
	_, err = parseRSAKey(g.PrivateKey)
	return err
}

// Token exchanges a freshly signed assertion for an access token. The call is
// bounded by ctx's deadline and at most 15s. Errors carry the token endpoint's
// HTTP status and RFC 6749 error fields (as *oauth2.RetrieveError), never its
// response body. An *http.Client set on ctx under oauth2.HTTPClient lends its
// Transport.
func (g JWTBearer) Token(ctx context.Context) (*Token, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	wait := maxTokenWait
	if dl, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(dl))
	}
	if wait <= 0 {
		return nil, context.DeadlineExceeded
	}
	// jwt.Config posts without the request context, so only the client's timeout bounds the call.
	hc := &http.Client{Timeout: wait}
	if base, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && base != nil {
		hc.Transport = base.Transport
	}
	cfg := &jwt.Config{
		Email:        g.Issuer,
		Subject:      g.Subject,
		Audience:     g.Audience,
		PrivateKey:   []byte(g.PrivateKey),
		PrivateKeyID: g.PrivateKeyID,
		TokenURL:     g.TokenURL,
		Scopes:       g.Scopes,
	}
	tok, err := cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, hc)).Token()
	if err != nil {
		return nil, withoutBody(err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("oauth: token endpoint returned no access token")
	}
	return tok, nil
}

// withoutBody replaces a token-endpoint error with one carrying only its status
// and RFC 6749 error fields: the endpoint is caller-supplied and its body may
// echo the request.
func withoutBody(err error) error {
	var re *oauth2.RetrieveError
	if !errors.As(err, &re) || re.Response == nil {
		return err
	}
	var fields struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(re.Body, &fields)
	return &oauth2.RetrieveError{
		Response:         &http.Response{Status: re.Response.Status, StatusCode: re.Response.StatusCode},
		ErrorCode:        fields.Error,
		ErrorDescription: fields.Description,
	}
}

func parseRSAKey(p string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errors.New("oauth: jwtBearer.privateKey is not PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("oauth: jwtBearer.privateKey is not an RSA key")
	}
	if rk, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return rk, nil
	}
	return nil, errors.New("oauth: jwtBearer.privateKey is not an RSA PKCS#8 or PKCS#1 key")
}

// Credential is the stored oauth credential: a token, plus the JWT-bearer
// grant that mints the next one when the credential is a service account.
type Credential struct {
	Token
	JWTBearer *JWTBearer `json:"jwtBearer,omitempty"`
}

// serviceAccountKeyFile is the key-file shape of a service account, plus the
// scopes array the caller adds to it.
type serviceAccountKeyFile struct {
	Type         string   `json:"type"`
	ClientEmail  string   `json:"client_email"`
	PrivateKey   string   `json:"private_key"`
	PrivateKeyID string   `json:"private_key_id"`
	TokenURI     string   `json:"token_uri"`
	Scopes       []string `json:"scopes"`
}

// ParseCredential reads a stored or submitted credential in one of three
// forms: a token blob (access_token or refresh_token), the Credential shape
// with jwtBearer, or a service-account key file ("type": "service_account")
// with an added scopes array. A JWT-bearer grant is validated; errors never
// include the input.
func ParseCredential(raw []byte) (*Credential, error) {
	var keyFile serviceAccountKeyFile
	if err := json.Unmarshal(raw, &keyFile); err != nil {
		return nil, fmt.Errorf("oauth: credential is not a JSON object: %w", err)
	}
	if keyFile.Type == "service_account" {
		g := &JWTBearer{
			Issuer:       keyFile.ClientEmail,
			PrivateKey:   keyFile.PrivateKey,
			PrivateKeyID: keyFile.PrivateKeyID,
			TokenURL:     keyFile.TokenURI,
			Scopes:       keyFile.Scopes,
		}
		if err := g.Validate(); err != nil {
			return nil, err
		}
		return &Credential{JWTBearer: g}, nil
	}
	var c Credential
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("oauth: credential: %w", err)
	}
	if c.JWTBearer != nil {
		if err := c.JWTBearer.Validate(); err != nil {
			return nil, err
		}
		return &c, nil
	}
	if c.AccessToken == "" && c.RefreshToken == "" {
		return nil, errors.New("oauth: credential has no access_token, refresh_token or jwtBearer")
	}
	return &c, nil
}
