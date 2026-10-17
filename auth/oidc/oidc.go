// Package oidc is an OpenID Connect relying party for browser login: Start redirects to the provider's hosted login, Callback exchanges the authorization code and hands the caller's claims to the product.
//
// Any provider publishing an OpenID discovery document whose issuer equals the configured issuer, give or take a trailing slash, works. The product owns accounts and sessions through the Provision and Login hooks; this package stores nothing. State, the PKCE verifier and the nonce travel in a short-lived SameSite=Lax cookie rather than a session, because the callback arrives as a cross-site top-level navigation that a SameSite=Strict session cookie does not accompany.
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
	// Issuer is the provider's issuer URL; endpoints and signing keys are discovered from its metadata. It must equal the metadata's issuer or differ from it only by one trailing slash; id_tokens must carry the metadata's issuer exactly.
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

// Codes of the failures this package creates.
const (
	CodeIDPError        = "idp_error"
	CodeFlowExpired     = "flow_expired"
	CodeStateMismatch   = "state_mismatch"
	CodeMissingCode     = "missing_code"
	CodeExchangeFailed  = "exchange_failed"
	CodeInvalidIdentity = "invalid_identity"
	CodeIDPUnavailable  = "idp_unavailable"
	CodeLoginFailed     = "login_failed"
	CodeInvalidReturnTo = "invalid_return_to"
)

// Failure is a refused login: the callback answers Status with Message. Code is a stable machine-readable reason, Name who was refused as far as known ("" until the provider has named anyone), Err the cause. Provision and Login return one to choose the response.
type Failure struct {
	Status  int
	Code    string
	Message string
	Name    string
	// Return is the return target of this login once the callback has read and re-validated it from the flow cookie, "" otherwise. The callback sets it, overwriting any value a hook gave.
	Return string
	Err    error
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
	// ReturnParam names the Start query parameter holding where the browser goes after this login instead of PostLoginURL; it defaults to "rd". Without ValidateReturn only a path on this origin is honoured, anything else is ignored.
	ReturnParam string
	// ValidateReturn, when set, decides which ReturnParam values are honoured and returns the target to store. Start refuses a login whose value it rejects with CodeInvalidReturnTo, and the callback checks the stored target with it again, because the flow cookie is not signed.
	ValidateReturn func(r *http.Request, raw string) (target string, ok bool)
	// Provision returns the product's user for c, creating it if the product's rules allow.
	Provision func(ctx context.Context, c Claims) (U, error)
	// Login starts the product's session for u.
	Login func(w http.ResponseWriter, r *http.Request, u U, c Claims) error
	// OnDenied observes each callback that refuses a login attempt, before the error response is written. A provider that cannot be discovered refuses no attempt and is not reported.
	OnDenied func(r *http.Request, f *Failure)
	// Render, when set, writes the response for each Failure: the callback's refusals, its discovery failure and a return target Start refuses. Unset, the response is f.Message as plain text with f.Status.
	Render func(w http.ResponseWriter, r *http.Request, f *Failure)

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
	ret, ok := rp.startReturn(r)
	if !ok {
		rp.respond(w, r, &Failure{Status: http.StatusBadRequest, Code: CodeInvalidReturnTo, Message: "return target not allowed"})
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

	rp.setFlowCookie(w, flowState{State: state, Verifier: verifier, Nonce: nonce, Return: ret})
	http.Redirect(w, r, authURL, http.StatusFound)
}

// Callback completes the login the provider redirected back: it checks state, exchanges the code, reads the claims, then runs Provision and Login and redirects to the return target Start accepted, checked again, or PostLoginURL without one.
func (rp *RelyingParty[U]) Callback(w http.ResponseWriter, r *http.Request) {
	cfg := rp.Config()
	if cfg == nil {
		http.Error(w, "oidc login is not enabled", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		slog.Warn("oidc callback: provider returned error", "error", e, "description", q.Get("error_description"))
		f := &Failure{Status: http.StatusBadGateway, Code: CodeIDPError, Message: "login failed at identity provider: " + e}
		// The flow cookie is read but kept, as before, only to learn where to send the browser.
		if fs, ok := rp.readFlowCookie(r); ok {
			f.Return = rp.storedReturn(r, fs.Return)
		}
		rp.deny(w, r, f)
		return
	}

	fs, ok := rp.takeFlowCookie(w, r)
	if !ok {
		rp.deny(w, r, &Failure{Status: http.StatusBadRequest, Code: CodeFlowExpired, Message: "login flow expired — start again"})
		return
	}
	ret := rp.storedReturn(r, fs.Return)
	refuse := func(f *Failure) {
		withReturn := *f
		withReturn.Return = ret
		rp.deny(w, r, &withReturn)
	}
	qstate := q.Get("state")
	if qstate == "" || subtle.ConstantTimeCompare([]byte(qstate), []byte(fs.State)) != 1 {
		refuse(&Failure{Status: http.StatusBadRequest, Code: CodeStateMismatch, Message: "state mismatch"})
		return
	}
	code := q.Get("code")
	if code == "" {
		refuse(&Failure{Status: http.StatusBadRequest, Code: CodeMissingCode, Message: "missing code"})
		return
	}

	p, oc, err := rp.oauth2Config(r.Context(), cfg)
	if err != nil {
		slog.Error("oidc callback: discovery failed", "issuer", cfg.Issuer, "err", err)
		rp.respond(w, r, &Failure{Status: http.StatusBadGateway, Code: CodeIDPUnavailable, Message: "identity provider unavailable", Return: ret, Err: err})
		return
	}
	tok, err := oc.Exchange(r.Context(), code, oauth2.VerifierOption(fs.Verifier))
	if err != nil {
		slog.Error("oidc callback: code exchange failed", "err", err)
		refuse(&Failure{Status: http.StatusBadGateway, Code: CodeExchangeFailed, Message: "code exchange failed", Err: err})
		return
	}
	claims, err := claimsFromToken(r.Context(), tok, p, cfg, fs.Nonce)
	if err != nil {
		slog.Error("oidc callback: id_token unusable", "err", err)
		msg := "identity provider returned no usable identity"
		if errors.Is(err, errInvalidIDToken) {
			msg = "identity provider returned an invalid id_token"
		}
		refuse(&Failure{Status: http.StatusBadGateway, Code: CodeInvalidIdentity, Message: msg, Err: err})
		return
	}

	u, err := rp.Provision(r.Context(), claims)
	if err != nil {
		refuse(hookFailure(err, "account provisioning failed", claims))
		return
	}
	if err := rp.Login(w, r, u, claims); err != nil {
		refuse(hookFailure(err, "login failed", claims))
		return
	}
	target := cfg.PostLoginURL
	if target == "" {
		target = "/"
	}
	if ret != "" {
		target = ret
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// hookFailure keeps a hook's own Failure and turns any other error into a 500.
func hookFailure(err error, message string, c Claims) *Failure {
	if f, ok := errors.AsType[*Failure](err); ok {
		return f
	}
	slog.Error("oidc callback: "+message, "err", err)
	return &Failure{Status: http.StatusInternalServerError, Code: CodeLoginFailed, Message: message, Name: c.Subject, Err: err}
}

func (rp *RelyingParty[U]) deny(w http.ResponseWriter, r *http.Request, f *Failure) {
	if rp.OnDenied != nil {
		rp.OnDenied(r, f)
	}
	rp.respond(w, r, f)
}

func (rp *RelyingParty[U]) respond(w http.ResponseWriter, r *http.Request, f *Failure) {
	if rp.Render != nil {
		rp.Render(w, r, f)
		return
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
