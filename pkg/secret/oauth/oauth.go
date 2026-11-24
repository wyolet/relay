// Package oauth resolves OAuth credentials for the relay's secret layer. The
// stored secret is an OAuth token blob (access + refresh + expiry, the
// sdk/oauth.Token wire shape) kept by the same stored backend as KindStored.
// Resolve returns the access token, transparently refreshing via the refresh
// token when it has expired and re-persisting the (often rotated) blob. A
// blob carrying a jwtBearer grant (a service account) mints its next token
// from that grant instead and needs no provider config.
//
// It is deliberately off the hot path: resolution happens at snapshot load and
// during KeyAgent heal after an upstream 401, never per request. Concurrent
// resolves of the same credential single-flight to one refresh.
//
// Layering: imports sdk/oauth (the flow machinery) + pkg/secret (the Ref/Vault
// contract). It never imports app/ — the provider config is injected as a plain
// lookup func by the composition root, which reads the live oauth:<provider>
// settings section.
package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"github.com/wyolet/relay/pkg/publicaddr"
	sdkoauth "github.com/wyolet/relay/sdk/oauth"
	"github.com/wyolet/relay/secret"
)

// Vault is the encrypt/decrypt persistence the resolver delegates to — the
// stored backend in practice. The OAuth blob round-trips as a KindStored
// value; Create only replaces an existing blob, keeping where it is stored.
type Vault interface {
	Resolve(ctx context.Context, ref secret.Ref) ([]byte, error)
	Create(ctx context.Context, id string, plaintext []byte) (secret.Ref, error)
}

// ProviderConfigFunc returns the OAuth config for a provider name (the
// oauth:<provider> settings section), or ok=false when unconfigured.
type ProviderConfigFunc func(provider string) (sdkoauth.ProviderConfig, bool)

// Resolver implements secret.Resolver for KindOAuth.
type Resolver struct {
	vault Vault
	cfg   ProviderConfigFunc
	sf    singleflight.Group
	now   func() time.Time
	skew  time.Duration
	// client carries every JWT-bearer exchange; see exchangeClient.
	client *http.Client
	// tokenHosts are the only token URL hosts a JWT-bearer exchange may call.
	tokenHosts map[string]struct{}

	failuresMu sync.Mutex
	failures   map[string]exchangeFailure
}

var _ secret.Resolver = (*Resolver)(nil)

// Reads resolve every key in turn and the token URL is caller-chosen, so a
// failing JWT-bearer exchange is retried on reads only after a backoff, and
// each read exchange is short.
const (
	firstBackoff = 30 * time.Second
	maxBackoff   = 10 * time.Minute
	readWait     = 5 * time.Second
)

// exchangeFailure is the last failed read exchange of one credential, as
// stored in blob (a new value starts over).
type exchangeFailure struct {
	blob  [sha256.Size]byte
	err   error
	wait  time.Duration
	until time.Time
}

// backedOff returns the remembered error while the credential stored as blob
// is inside its backoff.
func (r *Resolver) backedOff(id string, blob [sha256.Size]byte) error {
	r.failuresMu.Lock()
	defer r.failuresMu.Unlock()
	f, ok := r.failures[id]
	if !ok || f.blob != blob || !r.now().Before(f.until) {
		return nil
	}
	return f.err
}

// recordExchange forgets the credential's failures on success and otherwise
// doubles its backoff, from firstBackoff up to maxBackoff. Adding a failure
// drops every entry whose backoff ended more than maxBackoff ago, so ids that
// stopped failing, or no longer exist, do not pile up.
func (r *Resolver) recordExchange(id string, blob [sha256.Size]byte, err error) {
	if err == nil {
		r.forget(id)
		return
	}
	r.failuresMu.Lock()
	defer r.failuresMu.Unlock()
	now := r.now()
	for other, f := range r.failures {
		if now.After(f.until.Add(maxBackoff)) {
			delete(r.failures, other)
		}
	}
	wait := firstBackoff
	if f, ok := r.failures[id]; ok && f.blob == blob {
		wait = min(2*f.wait, maxBackoff)
	}
	if r.failures == nil {
		r.failures = map[string]exchangeFailure{}
	}
	r.failures[id] = exchangeFailure{blob: blob, err: err, wait: wait, until: now.Add(wait)}
}

// forget drops the credential's remembered failure after any exchange for it succeeds.
func (r *Resolver) forget(id string) {
	r.failuresMu.Lock()
	delete(r.failures, id)
	r.failuresMu.Unlock()
}

// Option configures a Resolver.
type Option func(*Resolver)

// AllowLoopback admits token URLs at loopback addresses, trusting tlsConfig
// for them; every other refused address and every redirect stay refused. For
// tests against a local token endpoint.
func AllowLoopback(tlsConfig *tls.Config) Option {
	return func(r *Resolver) { r.client = exchangeClient(true, tlsConfig) }
}

// TokenHosts sets the token URL hosts JWT-bearer credentials may name,
// matched exactly (case-insensitive) against the URL's hostname. Without it
// none is allowed, so service-account credentials are refused.
func TokenHosts(hosts []string) Option {
	return func(r *Resolver) {
		r.tokenHosts = make(map[string]struct{}, len(hosts))
		for _, h := range hosts {
			r.tokenHosts[strings.ToLower(h)] = struct{}{}
		}
	}
}

// unlistedHostError is a JWT-bearer token URL whose host is not in the
// operator's list; host is "" when the URL does not parse.
type unlistedHostError struct {
	host    string
	noHosts bool
}

func (e unlistedHostError) Error() string {
	if e.noHosts {
		return "service-account credentials are off: RELAY_OAUTH_TOKEN_HOSTS lists no token host"
	}
	return fmt.Sprintf("the token URL host %q is not in RELAY_OAUTH_TOKEN_HOSTS", e.host)
}

func (r *Resolver) checkTokenHost(tokenURL string) error {
	if len(r.tokenHosts) == 0 {
		return unlistedHostError{noHosts: true}
	}
	u, err := url.Parse(tokenURL)
	if err != nil {
		return unlistedHostError{}
	}
	host := strings.ToLower(u.Hostname())
	if _, ok := r.tokenHosts[host]; !ok {
		return unlistedHostError{host: host}
	}
	return nil
}

// NewResolver builds an OAuth resolver. vault provides the encrypted
// persistence; cfg looks up the provider's flow config.
func NewResolver(vault Vault, cfg ProviderConfigFunc, opts ...Option) *Resolver {
	r := &Resolver{vault: vault, cfg: cfg, now: time.Now, skew: time.Minute, client: exchangeClient(false, nil)}
	for _, o := range opts {
		o(r)
	}
	return r
}

var errRedirect = errors.New("secret/oauth: the token endpoint answered with a redirect")

// exchangeClient is the client of every JWT-bearer exchange. Its token URL is
// caller-supplied, so it connects only to public addresses (checked on the
// resolved IP when dialing), through no proxy, follows no redirect and keeps
// no idle connection.
func exchangeClient(allowLoopback bool, tlsConfig *tls.Config) *http.Client {
	control := publicaddr.DialControl
	if allowLoopback {
		control = func(network, address string, c syscall.RawConn) error {
			if ap, err := netip.ParseAddrPort(address); err == nil && ap.Addr().Unmap().IsLoopback() {
				return nil
			}
			return publicaddr.DialControl(network, address, c)
		}
	}
	dialer := &net.Dialer{Timeout: prepareWait, Control: control}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			TLSClientConfig:     tlsConfig,
			TLSHandshakeTimeout: prepareWait,
			ForceAttemptHTTP2:   true,
			DisableKeepAlives:   true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
}

// exchange mints a token from g over the guarded client, whatever client ctx
// carries, and only when g's token host is listed.
func (r *Resolver) exchange(ctx context.Context, g *sdkoauth.JWTBearer) (*sdkoauth.Token, error) {
	if err := r.checkTokenHost(g.TokenURL); err != nil {
		return nil, err
	}
	return g.Token(context.WithValue(ctx, oauth2.HTTPClient, r.client))
}

// Resolve returns the current access token for an OAuth credential, refreshing
// first if it has expired (within a small skew). A refresh that yields a new
// token re-persists the blob so the rotated refresh token is not lost.
func (r *Resolver) Resolve(ctx context.Context, ref secret.Ref) ([]byte, error) {
	if ref.Kind != secret.KindOAuth {
		return nil, fmt.Errorf("secret/oauth: wrong kind %q", ref.Kind)
	}
	cred, _, err := r.load(ctx, ref.ID)
	if err != nil {
		return nil, err
	}
	if r.valid(&cred.Token) {
		return []byte(cred.AccessToken), nil
	}
	v, err, _ := r.sf.Do(ref.ID, func() (any, error) {
		// Re-load inside the flight: a concurrent refresh may have already
		// written a fresh blob while we waited for the lock.
		cur, blob, err := r.load(ctx, ref.ID)
		if err != nil {
			return nil, err
		}
		if r.valid(&cur.Token) {
			return cur.AccessToken, nil
		}
		if cur.JWTBearer == nil {
			access, _, err := r.refresh(ctx, ref, cur)
			return access, err
		}
		if err := r.backedOff(ref.ID, blob); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, readWait)
		defer cancel()
		access, _, err := r.refresh(ctx, ref, cur)
		r.recordExchange(ref.ID, blob, err)
		return access, err
	})
	if err != nil {
		return nil, err
	}
	return []byte(v.(string)), nil
}

// load reads the stored credential and the digest of its blob.
func (r *Resolver) load(ctx context.Context, id string) (*sdkoauth.Credential, [sha256.Size]byte, error) {
	raw, err := r.vault.Resolve(ctx, secret.Ref{Kind: secret.KindStored, ID: id})
	if err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("secret/oauth: load %q: %w", id, err)
	}
	var cred sdkoauth.Credential
	if err := json.Unmarshal(raw, &cred); err != nil {
		return nil, [sha256.Size]byte{}, fmt.Errorf("secret/oauth: parse stored token %q: %w", id, err)
	}
	return &cred, sha256.Sum256(raw), nil
}

func (r *Resolver) refresh(ctx context.Context, ref secret.Ref, cur *sdkoauth.Credential) (string, bool, error) {
	nt, rotated, err := r.refreshTok(ctx, ref, cur)
	if err != nil {
		return "", false, err
	}
	return nt.AccessToken, rotated, nil
}

func (r *Resolver) refreshTok(ctx context.Context, ref secret.Ref, cur *sdkoauth.Credential) (*sdkoauth.Token, bool, error) {
	tok := &cur.Token
	var nt *sdkoauth.Token
	if cur.JWTBearer != nil {
		var err error
		if nt, err = r.exchange(ctx, cur.JWTBearer); err == nil && r.shortLived(nt) {
			err = errShortLifetime
		}
		if err != nil {
			slog.Warn("secret/oauth: token exchange failed", "id", ref.ID, "err", err)
			return nil, false, fmt.Errorf("secret/oauth: %q: %w", ref.ID, exchangeError{err})
		}
		r.forget(ref.ID)
	} else {
		cfg, ok := r.cfg(ref.Provider)
		if !ok {
			return nil, false, fmt.Errorf("secret/oauth: no config for provider %q", ref.Provider)
		}
		if tok.RefreshToken == "" {
			return nil, false, fmt.Errorf("secret/oauth: token %q expired with no refresh token", ref.ID)
		}
		var err error
		if nt, err = sdkoauth.New(cfg.OAuth2()).Refresh(ctx, tok); err != nil {
			return nil, false, fmt.Errorf("secret/oauth: refresh %q: %w", ref.ID, err)
		}
	}
	if nt.AccessToken == "" {
		return nil, false, fmt.Errorf("secret/oauth: refresh %q returned empty access token", ref.ID)
	}
	rotated := nt.AccessToken != tok.AccessToken || nt.RefreshToken != tok.RefreshToken || !nt.Expiry.Equal(tok.Expiry)
	if rotated {
		blob, err := json.Marshal(sdkoauth.Credential{Token: *nt, JWTBearer: cur.JWTBearer})
		if err != nil {
			return nil, false, fmt.Errorf("secret/oauth: marshal refreshed token: %w", err)
		}
		if _, err := r.vault.Create(ctx, ref.ID, blob); err != nil {
			return nil, false, fmt.Errorf("secret/oauth: persist refreshed token %q: %w", ref.ID, err)
		}
	}
	return nt, rotated, nil
}

// Renew proactively refreshes the credential when its expiry falls within
// lead, persisting the rotated blob. Reports whether a new blob was
// persisted and the credential's current expiry. A token with no expiry,
// or one still comfortably valid, is a no-op. Callers coordinating across
// processes must hold their own lock — the in-process single-flight only
// dedupes within this resolver.
func (r *Resolver) Renew(ctx context.Context, ref secret.Ref, lead time.Duration) (bool, time.Time, error) {
	if ref.Kind != secret.KindOAuth {
		return false, time.Time{}, fmt.Errorf("secret/oauth: wrong kind %q", ref.Kind)
	}
	due := func(tok *sdkoauth.Token) bool {
		return !tok.Expiry.IsZero() && tok.Expiry.Before(r.now().Add(lead))
	}
	cred, _, err := r.load(ctx, ref.ID)
	if err != nil {
		return false, time.Time{}, err
	}
	if !due(&cred.Token) {
		return false, cred.Expiry, nil
	}
	type renewed struct {
		rotated bool
		expiry  time.Time
	}
	v, err, _ := r.sf.Do("renew:"+ref.ID, func() (any, error) {
		cur, _, err := r.load(ctx, ref.ID)
		if err != nil {
			return renewed{}, err
		}
		if !due(&cur.Token) {
			return renewed{false, cur.Expiry}, nil
		}
		// The oauth2 TokenSource only exchanges a token its own Valid()
		// deems expired (~10s delta) — a proactive renewal inside the lead
		// window must force the exchange, so hand it a copy with the expiry
		// already past. Rotation still compares against the persisted blob.
		force := *cur
		force.Expiry = r.now().Add(-time.Hour)
		nt, rotated, err := r.refreshTok(ctx, ref, &force)
		if err != nil {
			return renewed{}, err
		}
		return renewed{rotated, nt.Expiry}, nil
	})
	if err != nil {
		return false, time.Time{}, err
	}
	res := v.(renewed)
	return res.rotated, res.expiry, nil
}

// IsPermanent reports whether a refresh error is unrecoverable without
// operator re-authorization (the provider rejected the grant itself, e.g.
// a revoked or reused refresh token), as opposed to a transient
// network/server failure worth retrying. A success without an access token is
// never permanent here: a renewal retries it, as it does a token URL the
// exchange client refuses (a non-public address, a redirect), since DNS
// answers change. A token URL whose host is not listed is permanent.
func IsPermanent(err error) bool {
	if errors.As(err, new(unlistedHostError)) {
		return true
	}
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		switch re.ErrorCode {
		case "invalid_grant", "invalid_client", "unauthorized_client":
			return true
		}
		// Some providers omit the RFC 6749 error code; a 4xx token-endpoint
		// answer is a grant problem either way (5xx/network, a timeout or a
		// rate limit stay transient).
		if re.Response == nil {
			return false
		}
		switch code := re.Response.StatusCode; code {
		case http.StatusRequestTimeout, http.StatusTooManyRequests:
			return false
		default:
			return code >= 400 && code < 500
		}
	}
	return false
}

func refusedURL(err error) bool {
	return errors.Is(err, publicaddr.ErrRefused) || errors.Is(err, errRedirect)
}

// exchangeError is a failed JWT-bearer exchange of a stored credential as
// anyone reading the key sees it (its unresolved reason, its renewal error):
// a short classified reason, never the network error text, which can name the
// addresses a host resolved to.
type exchangeError struct{ err error }

func (e exchangeError) Error() string { return "token exchange failed: " + exchangeReason(e.err) }
func (e exchangeError) Unwrap() error { return e.err }

func exchangeReason(err error) string {
	var unlisted unlistedHostError
	var re *oauth2.RetrieveError
	var netErr net.Error
	switch {
	case errors.As(err, &unlisted):
		return unlisted.Error()
	case errors.Is(err, publicaddr.ErrRefused):
		return "the token URL does not resolve to a public address"
	case errors.Is(err, errRedirect):
		return "the token endpoint answered with a redirect"
	case errors.Is(err, sdkoauth.ErrNoAccessToken):
		return "the token endpoint returned no access token"
	case errors.Is(err, errShortLifetime):
		return errShortLifetime.Error()
	case errors.As(err, &re) && re.Response != nil:
		reason := fmt.Sprintf("the token endpoint answered HTTP %d", re.Response.StatusCode)
		if re.ErrorCode != "" {
			reason += " (" + re.ErrorCode + ")"
		}
		return reason
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "the token endpoint timed out"
	default:
		return "the token endpoint could not be reached or gave no usable answer"
	}
}

// rejectedError is the refusal of a new credential, worded for the operator: for a token endpoint's answer, its HTTP status and RFC 6749 error fields; never an address the URL resolved to.
type rejectedError struct{ err error }

func (e rejectedError) Error() string {
	var unlisted unlistedHostError
	switch {
	case errors.As(e.err, &unlisted):
		return unlisted.Error()
	case errors.Is(e.err, sdkoauth.ErrNoAccessToken):
		return sdkoauth.ErrNoAccessToken.Error()
	case errors.Is(e.err, publicaddr.ErrRefused):
		return "the token URL is refused: relay calls a token endpoint only at a public address"
	case errors.Is(e.err, errRedirect):
		return "the token endpoint answered with a redirect, which relay does not follow"
	}
	msg := "the token endpoint rejected the credential"
	var re *oauth2.RetrieveError
	if !errors.As(e.err, &re) {
		return msg
	}
	if re.Response != nil {
		msg += fmt.Sprintf(" with HTTP %d", re.Response.StatusCode)
	}
	if re.ErrorCode != "" {
		msg += ": " + re.ErrorCode
	}
	if re.ErrorDescription != "" {
		msg += " (" + re.ErrorDescription + ")"
	}
	return msg
}

func (e rejectedError) Unwrap() error { return e.err }

// minTokenLifetime is the shortest token lifetime a JWT-bearer exchange may
// return: a token without an expiry would never be renewed, and one inside the
// refresher's lead would be renewed on every sweep.
const minTokenLifetime = defaultLead + 5*time.Minute

// errShortLifetime refuses a new credential on save; a renewal retries it and keeps the current token.
var errShortLifetime = fmt.Errorf("the token endpoint returned a token with no expiry (expires_in) or one valid for under %d minutes", int(minTokenLifetime/time.Minute))

func (r *Resolver) shortLived(tok *sdkoauth.Token) bool {
	return tok.Expiry.IsZero() || tok.Expiry.Before(r.now().Add(minTokenLifetime))
}

// prepareWait bounds the token exchange a write runs to check a new credential.
const prepareWait = 10 * time.Second

// PrepareValue checks a new oauth host-key value before it is stored and
// returns what to store. A token blob is returned byte-identical. A
// service-account credential (a key file or the Credential shape) is
// exchanged once: a rejected grant fails the write, while a transient failure
// stores the credential without a token, minted on the next resolve. Every
// error is a refusal of the value, worded for the operator and never quoting
// it. id names the host key the value is for.
func (r *Resolver) PrepareValue(ctx context.Context, id string, raw []byte) ([]byte, error) {
	cred, err := sdkoauth.ParseCredential(raw)
	if err != nil {
		return nil, err
	}
	if cred.JWTBearer == nil {
		return raw, nil
	}
	ctx, cancel := context.WithTimeout(ctx, prepareWait)
	defer cancel()
	next := sdkoauth.Credential{JWTBearer: cred.JWTBearer}
	tok, err := r.exchange(ctx, cred.JWTBearer)
	switch {
	case err == nil && r.shortLived(tok):
		return nil, errShortLifetime
	case err == nil:
		next.Token = *tok
		r.forget(id)
	case IsPermanent(err), errors.Is(err, sdkoauth.ErrNoAccessToken), refusedURL(err):
		return nil, rejectedError{err}
	default:
		slog.Warn("secret/oauth: token exchange failed; storing the credential without a token", "err", err)
	}
	return json.Marshal(next)
}

func (r *Resolver) valid(tok *sdkoauth.Token) bool {
	if tok.AccessToken == "" {
		return false
	}
	if tok.Expiry.IsZero() {
		return true
	}
	return tok.Expiry.After(r.now().Add(r.skew))
}
