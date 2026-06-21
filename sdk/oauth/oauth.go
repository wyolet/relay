// Package oauth provides minimal, vendor-neutral OAuth 2.0 flow helpers for
// acquiring and refreshing provider credentials. It is a thin convenience
// layer over golang.org/x/oauth2 covering the authorization-code grant with
// PKCE (RFC 7636) and the device authorization grant (RFC 8628).
//
// It is shared by SDK consumers driving an interactive login and by the relay
// server doing server-side refresh. It owns flow *machinery* only — acquiring
// and refreshing tokens on demand — never lifecycle: it starts no background
// goroutines and persists nothing. Storage and refresh scheduling are the
// caller's responsibility.
//
// Vendor specifics (endpoints, client id, scopes) live in
// sdk/adapters/<vendor>/; construct an *oauth2.Config there and pass it here.
// This package imports nothing from app/ or internal/ (SDK module purity).
package oauth

import (
	"context"

	"golang.org/x/oauth2"
)

// Token is the serializable credential a flow produces. It is the wire
// contract between an interactive-login client and whatever stores the
// credential (e.g. the relay oauth host-key value). It is an alias of
// oauth2.Token so a stored token round-trips back into Refresh without
// conversion; its JSON carries access_token, refresh_token, and expiry.
type Token = oauth2.Token

// Flow drives OAuth grants for a single provider configuration. It holds no
// per-request state and is safe to reuse and share across goroutines.
type Flow struct {
	cfg *oauth2.Config
}

// New returns a Flow bound to cfg.
func New(cfg *oauth2.Config) *Flow { return &Flow{cfg: cfg} }

// AuthorizeURL builds the authorization-code URL to send the user to (browser /
// redirect contexts) and returns the PKCE verifier the caller must retain and
// hand to Exchange. A fresh verifier is generated per call, per RFC 7636.
func (f *Flow) AuthorizeURL(state string, opts ...oauth2.AuthCodeOption) (url, verifier string) {
	verifier = oauth2.GenerateVerifier()
	opts = append(opts, oauth2.S256ChallengeOption(verifier))
	return f.cfg.AuthCodeURL(state, opts...), verifier
}

// Exchange swaps an authorization code, plus the matching PKCE verifier from
// AuthorizeURL, for a token.
func (f *Flow) Exchange(ctx context.Context, code, verifier string, opts ...oauth2.AuthCodeOption) (*Token, error) {
	opts = append(opts, oauth2.VerifierOption(verifier))
	return f.cfg.Exchange(ctx, code, opts...)
}

// DeviceAuth begins the device authorization grant (RFC 8628), for headless /
// CLI contexts with no browser to redirect. The returned response carries the
// user code and verification URI to display to the user.
func (f *Flow) DeviceAuth(ctx context.Context, opts ...oauth2.AuthCodeOption) (*oauth2.DeviceAuthResponse, error) {
	return f.cfg.DeviceAuth(ctx, opts...)
}

// DeviceToken polls the token endpoint until the user completes authorization
// or the device code expires. It blocks; x/oauth2 runs the RFC 8628 polling
// loop (honouring authorization_pending / slow_down) internally.
func (f *Flow) DeviceToken(ctx context.Context, da *oauth2.DeviceAuthResponse, opts ...oauth2.AuthCodeOption) (*Token, error) {
	return f.cfg.DeviceAccessToken(ctx, da, opts...)
}

// Refresh returns a non-expired token. A still-valid token is returned
// unchanged with no network call; an expired one is exchanged via its refresh
// token (x/oauth2 carries the refresh token forward if the response omits a
// new one). It never mutates the input and never persists — the caller owns
// storage. Errors if the token is expired and has no refresh token.
func (f *Flow) Refresh(ctx context.Context, tok *Token) (*Token, error) {
	return f.cfg.TokenSource(ctx, tok).Token()
}
