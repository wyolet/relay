package oauth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sync/atomic"
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
// Transport. A success response without an access token is ErrNoAccessToken.
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
	status := &statusRecorder{next: http.DefaultTransport}
	if base, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && base != nil && base.Transport != nil {
		status.next = base.Transport
	}
	// jwt.Config posts without the request context, so only the client's timeout bounds the call.
	hc := &http.Client{Timeout: wait, Transport: status}
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
	// A success the grant cannot read a token from (an id_token alone, say) is the endpoint refusing what was asked, not an outage.
	if status.code >= 200 && status.code < 300 && status.read.Load() && (err != nil || tok.AccessToken == "") {
		return nil, ErrNoAccessToken
	}
	if err != nil {
		return nil, withoutBody(err)
	}
	if tok.AccessToken == "" {
		return nil, ErrNoAccessToken
	}
	return tok, nil
}

// ErrNoAccessToken is a token endpoint answering success without an access
// token, as one does for scopes it does not grant the account.
var ErrNoAccessToken = errors.New("oauth: the token endpoint returned no access token; check the scopes")

// statusRecorder keeps the status of the one response a token exchange
// receives, and whether its body was read to the end: a body cut short by
// the timeout is an outage, not an answer.
type statusRecorder struct {
	next http.RoundTripper
	code int
	read atomic.Bool
}

func (s *statusRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.next.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	s.code = resp.StatusCode
	resp.Body = &eofBody{ReadCloser: resp.Body, read: &s.read}
	return resp, nil
}

type eofBody struct {
	io.ReadCloser
	read *atomic.Bool
}

func (b *eofBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.read.Store(true)
	}
	return n, err
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

var errNotCredential = errors.New("oauth: the credential is not a token blob, a jwtBearer credential or a service-account key file (a field has the wrong type)")

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
	// JSON decode errors quote the offending input, so they are not passed on.
	if !json.Valid(raw) {
		return nil, errors.New("oauth: the credential is not valid JSON")
	}
	var keyFile serviceAccountKeyFile
	if err := json.Unmarshal(raw, &keyFile); err != nil {
		return nil, errNotCredential
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
		return nil, errNotCredential
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
