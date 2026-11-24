package oauth

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
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
// Transport and CheckRedirect; their own errors are returned wrapped, so
// errors.Is sees them. A success response whose body is a JSON object without an access
// token is ErrNoAccessToken.
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
	// jwt.Config posts without the request context, so only the client's timeout bounds the call.
	hc := &http.Client{Timeout: wait, Transport: status}
	if base, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && base != nil {
		if base.Transport != nil {
			status.next = base.Transport
		}
		if base.CheckRedirect != nil {
			hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				err := base.CheckRedirect(req, via)
				if !errors.Is(err, http.ErrUseLastResponse) {
					status.err = err
				}
				return err
			}
		}
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
	if status.code >= 200 && status.code < 300 && (err != nil || tok.AccessToken == "") {
		if status.tokenlessObject() {
			return nil, ErrNoAccessToken
		}
		return nil, errUnreadableSuccess
	}
	if err != nil && status.err != nil {
		// jwt.Config flattens transport errors to text; the client's own error keeps its chain.
		return nil, fmt.Errorf("oauth: token request failed: %w", status.err)
	}
	if err != nil {
		return nil, withoutBody(err)
	}
	return tok, nil
}

// ErrNoAccessToken is a token endpoint answering success with a JSON object
// that holds no access token, as one does for a scope it does not grant the
// account (it may send only an id_token).
var ErrNoAccessToken = errors.New("oauth: the token endpoint returned no access token; check the scopes")

// errUnreadableSuccess is any other success without a usable token: a body
// that is not a JSON object (a maintenance page, say) or one cut short.
var errUnreadableSuccess = errors.New("oauth: the token endpoint answered success without a readable token")

// maxTokenBody is what x/oauth2/jwt reads of a token response.
const maxTokenBody = 1 << 20

// statusRecorder keeps the status and body of the one response a token
// exchange receives, so a success without a token can be told apart from an
// unreadable one.
type statusRecorder struct {
	next http.RoundTripper
	code int
	body *recordedBody
	// err is the transport's or the redirect policy's own error, if any.
	err error
}

func (s *statusRecorder) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := s.next.RoundTrip(r)
	if err != nil {
		s.err = err
		return nil, err
	}
	s.code = resp.StatusCode
	s.body = &recordedBody{ReadCloser: resp.Body}
	resp.Body = s.body
	return resp, nil
}

// tokenlessObject reports whether the whole response body was read and is a
// JSON object with no non-empty access_token string.
func (s *statusRecorder) tokenlessObject() bool {
	if s.body == nil || !s.body.complete || s.body.buf.Len() > maxTokenBody {
		return false
	}
	var members map[string]json.RawMessage
	if json.Unmarshal(s.body.buf.Bytes(), &members) != nil {
		return false
	}
	var access string
	_ = json.Unmarshal(members["access_token"], &access)
	return access == ""
}

type recordedBody struct {
	io.ReadCloser
	buf      bytes.Buffer
	complete bool
}

func (b *recordedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.buf.Len() <= maxTokenBody {
		b.buf.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		b.complete = true
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
// forms, told apart by its exact top-level members: the Credential shape
// (jwtBearer), a token blob (access_token or refresh_token; nothing else in it
// is decoded), or a service-account key file ("type": "service_account") with
// an added scopes array. A JWT-bearer grant is validated; errors never include
// the input.
func ParseCredential(raw []byte) (*Credential, error) {
	// JSON decode errors quote the offending input, so they are not passed on.
	if !json.Valid(raw) {
		return nil, errors.New("oauth: the credential is not valid JSON")
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, errors.New("oauth: the credential is not a JSON object")
	}
	_, hasAccess := members["access_token"]
	_, hasRefresh := members["refresh_token"]
	switch {
	case members["jwtBearer"] != nil:
		var c Credential
		if err := json.Unmarshal(raw, &c); err != nil || c.JWTBearer == nil {
			return nil, errNotCredential
		}
		if err := c.JWTBearer.Validate(); err != nil {
			return nil, err
		}
		return &c, nil
	case hasAccess || hasRefresh:
		var c Credential
		if hasAccess && json.Unmarshal(members["access_token"], &c.AccessToken) != nil ||
			hasRefresh && json.Unmarshal(members["refresh_token"], &c.RefreshToken) != nil {
			return nil, errors.New("oauth: access_token and refresh_token must be strings")
		}
		return &c, nil
	case string(members["type"]) == `"service_account"`:
		var keyFile serviceAccountKeyFile
		if err := json.Unmarshal(raw, &keyFile); err != nil {
			return nil, errNotCredential
		}
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
	default:
		return nil, errors.New("oauth: the credential has no access_token, refresh_token or jwtBearer and is not a service-account key file")
	}
}
