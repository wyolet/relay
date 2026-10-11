// Package oidc is an OpenID Connect relying party for browser login: Start redirects to the provider's hosted login, Callback exchanges the authorization code and hands the caller's claims to the product.
//
// Any provider publishing an OpenID discovery document whose issuer equals the configured issuer works. The product owns accounts and sessions through the Provision and Login hooks; this package stores nothing. State, the PKCE verifier and the nonce travel in a short-lived SameSite=Lax cookie rather than a session, because the callback arrives as a cross-site top-level navigation that a SameSite=Strict session cookie does not accompany.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Config is one provider registration. A RelyingParty reads it on every request, so a changed setting applies to the next login.
type Config struct {
	// Issuer is the provider's issuer URL; endpoints and signing keys are discovered from its metadata. It must equal the metadata's issuer exactly, trailing slash included.
	Issuer   string
	ClientID string
	// ClientSecret is sent in the token request's form parameters; empty for a public client, which relies on PKCE alone.
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	// AuthParams are extra authorization-URL query parameters some providers require.
	AuthParams map[string]string
	// GroupsClaim names the id_token claim holding the user's groups as a string array; empty reads none.
	GroupsClaim string
	// PostLoginURL is where the browser goes once Login succeeds; empty means "/".
	PostLoginURL string
}

// Failure is a refused login: the callback answers Status with Message. Name is who was refused as far as known ("" until the provider has named anyone), Err the cause. Provision and Login return one to choose the response.
type Failure struct {
	Status  int
	Message string
	Name    string
	Err     error
}

func (f *Failure) Error() string {
	if f.Err != nil {
		return f.Message + ": " + f.Err.Error()
	}
	return f.Message
}

func (f *Failure) Unwrap() error { return f.Err }

// RelyingParty serves Start and Callback for one product user type U. Config, Provision and Login are required.
type RelyingParty[U any] struct {
	// Config returns the current registration, or nil when login is disabled, which both handlers answer with 404.
	Config func() *Config
	// FlowCookie names the cookie carrying state and the PKCE verifier from Start to Callback; it defaults to "oidc_flow".
	FlowCookie string
	// CookieSecure marks the flow cookie HTTPS-only.
	CookieSecure bool
	// ReturnParam names the Start query parameter holding where the browser goes after this login instead of PostLoginURL; it defaults to "rd". Only a path on this origin is honoured, anything else is ignored.
	ReturnParam string
	// Provision returns the product's user for c, creating it if the product's rules allow.
	Provision func(ctx context.Context, c Claims) (U, error)
	// Login starts the product's session for u.
	Login func(w http.ResponseWriter, r *http.Request, u U, c Claims) error
	// OnDenied observes each callback that refuses a login attempt, before the error response is written. A provider that cannot be discovered refuses no attempt and is not reported.
	OnDenied func(r *http.Request, f *Failure)

	mu        sync.Mutex
	discovery map[string]cachedProvider
}

// Start redirects the browser to the provider's authorization endpoint and sets the flow cookie.
func (rp *RelyingParty[U]) Start(w http.ResponseWriter, r *http.Request) {
	cfg := rp.Config()
	if cfg == nil {
		http.Error(w, "oidc login is not enabled", http.StatusNotFound)
		return
	}
	_, oc, err := rp.oauth2Config(r.Context(), cfg)
	if err != nil {
		slog.Error("oidc start: discovery failed", "issuer", cfg.Issuer, "err", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}

	state, err := randomHex()
	if err != nil {
		http.Error(w, "entropy unavailable", http.StatusInternalServerError)
		return
	}
	nonce, err := randomHex()
	if err != nil {
		http.Error(w, "entropy unavailable", http.StatusInternalServerError)
		return
	}

	verifier := oauth2.GenerateVerifier()
	opts := make([]oauth2.AuthCodeOption, 0, len(cfg.AuthParams)+2)
	for k, v := range cfg.AuthParams {
		opts = append(opts, oauth2.SetAuthURLParam(k, v))
	}
	opts = append(opts, oauth2.S256ChallengeOption(verifier), gooidc.Nonce(nonce))
	authURL := oc.AuthCodeURL(state, opts...)

	fs := flowState{State: state, Verifier: verifier, Nonce: nonce}
	if rd := r.URL.Query().Get(rp.returnParamName()); safeReturnPath(rd) {
		fs.Return = rd
	}
	rp.setFlowCookie(w, fs)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// Callback completes the login the provider redirected back: it checks state, exchanges the code, reads the claims, then runs Provision and Login and redirects to the return path Start accepted, or PostLoginURL without one.
func (rp *RelyingParty[U]) Callback(w http.ResponseWriter, r *http.Request) {
	cfg := rp.Config()
	if cfg == nil {
		http.Error(w, "oidc login is not enabled", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		slog.Warn("oidc callback: provider returned error", "error", e, "description", q.Get("error_description"))
		rp.deny(w, r, &Failure{Status: http.StatusBadGateway, Message: "login failed at identity provider: " + e})
		return
	}

	fs, ok := rp.takeFlowCookie(w, r)
	if !ok {
		rp.deny(w, r, &Failure{Status: http.StatusBadRequest, Message: "login flow expired — start again"})
		return
	}
	qstate := q.Get("state")
	if qstate == "" || subtle.ConstantTimeCompare([]byte(qstate), []byte(fs.State)) != 1 {
		rp.deny(w, r, &Failure{Status: http.StatusBadRequest, Message: "state mismatch"})
		return
	}
	code := q.Get("code")
	if code == "" {
		rp.deny(w, r, &Failure{Status: http.StatusBadRequest, Message: "missing code"})
		return
	}

	p, oc, err := rp.oauth2Config(r.Context(), cfg)
	if err != nil {
		slog.Error("oidc callback: discovery failed", "issuer", cfg.Issuer, "err", err)
		http.Error(w, "identity provider unavailable", http.StatusBadGateway)
		return
	}
	tok, err := oc.Exchange(r.Context(), code, oauth2.VerifierOption(fs.Verifier))
	if err != nil {
		slog.Error("oidc callback: code exchange failed", "err", err)
		rp.deny(w, r, &Failure{Status: http.StatusBadGateway, Message: "code exchange failed", Err: err})
		return
	}
	claims, err := claimsFromToken(r.Context(), tok, p, cfg, fs.Nonce)
	if err != nil {
		slog.Error("oidc callback: id_token unusable", "err", err)
		msg := "identity provider returned no usable identity"
		if errors.Is(err, errInvalidIDToken) {
			msg = "identity provider returned an invalid id_token"
		}
		rp.deny(w, r, &Failure{Status: http.StatusBadGateway, Message: msg, Err: err})
		return
	}

	u, err := rp.Provision(r.Context(), claims)
	if err != nil {
		rp.deny(w, r, hookFailure(err, "account provisioning failed", claims))
		return
	}
	if err := rp.Login(w, r, u, claims); err != nil {
		rp.deny(w, r, hookFailure(err, "login failed", claims))
		return
	}
	target := cfg.PostLoginURL
	if target == "" {
		target = "/"
	}
	// The flow cookie is not signed, so its return path is checked again.
	if safeReturnPath(fs.Return) {
		target = fs.Return
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// hookFailure keeps a hook's own Failure and turns any other error into a 500.
func hookFailure(err error, message string, c Claims) *Failure {
	if f, ok := errors.AsType[*Failure](err); ok {
		return f
	}
	slog.Error("oidc callback: "+message, "err", err)
	return &Failure{Status: http.StatusInternalServerError, Message: message, Name: c.Subject, Err: err}
}

func (rp *RelyingParty[U]) deny(w http.ResponseWriter, r *http.Request, f *Failure) {
	if rp.OnDenied != nil {
		rp.OnDenied(r, f)
	}
	http.Error(w, f.Message, f.Status)
}

// oauth2Config returns cfg's discovered provider and shapes cfg and its endpoints for x/oauth2. Client credentials go in the form parameters, which a public client without a secret needs.
func (rp *RelyingParty[U]) oauth2Config(ctx context.Context, cfg *Config) (*gooidc.Provider, *oauth2.Config, error) {
	p, err := rp.provider(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	ep := p.Endpoint()
	return p, &oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Scopes:       cfg.Scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:   ep.AuthURL,
			TokenURL:  ep.TokenURL,
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}, nil
}

func randomHex() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
