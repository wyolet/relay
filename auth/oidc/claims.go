package oidc

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// iatLeeway is the clock skew allowed on iat, the leeway go-oidc applies to nbf.
const iatLeeway = 5 * time.Minute

// errInvalidIDToken marks an id_token that failed verification, as opposed to a verified one that names nobody.
var errInvalidIDToken = errors.New("id_token rejected")

// Claims is the identity a login produced.
//
// An id_token is verified per OIDC Core 3.1.3.7 before any claim is read: signature against the provider's JWKS (asymmetric algorithms only), iss, aud and azp, exp, iat and the nonce sent with the authorization request. A response with no id_token but an inline profile is trusted on the server-side code exchange alone: TLS, client authentication, state and PKCE. Bearer tokens presented by callers are a different surface.
type Claims struct {
	// Issuer is the configured issuer the login went through; a verified id_token's iss equals it.
	Issuer            string
	Subject           string
	Email             string
	PreferredUsername string
	// SessionID is the provider's session id (the sid claim), which a back-channel logout names.
	SessionID string
	// Groups holds the string values of Config.GroupsClaim.
	Groups []string
}

type idTokenPayload struct {
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Sid               string `json:"sid"`
	Azp               string `json:"azp"`
}

// claimsFromToken reads the caller's identity from the code-exchange response. Standard OIDC carries it as an id_token JWT, which must verify; some hosted user-management providers send no id_token and inline a profile object ({"user": {"id": ..., "email": ...}}) instead. The JWT is used whenever present.
func claimsFromToken(ctx context.Context, tok *oauth2.Token, p *gooidc.Provider, cfg *Config, nonce string) (Claims, error) {
	if raw, _ := tok.Extra("id_token").(string); raw != "" {
		idt, payload, err := verifyIDToken(ctx, p, cfg.ClientID, nonce, raw)
		if err != nil {
			return Claims{}, fmt.Errorf("%w: %w", errInvalidIDToken, err)
		}
		if idt.Subject == "" {
			return Claims{}, errors.New("id_token has no sub")
		}
		// The groups claim's name is configuration, so it is read from the generic decode.
		var all map[string]any
		_ = idt.Claims(&all)
		return Claims{
			Issuer:            cfg.Issuer,
			Subject:           idt.Subject,
			Email:             payload.Email,
			PreferredUsername: payload.PreferredUsername,
			SessionID:         payload.Sid,
			Groups:            stringsClaim(all, cfg.GroupsClaim),
		}, nil
	}
	if m, ok := tok.Extra("user").(map[string]any); ok {
		c := Claims{Issuer: cfg.Issuer}
		c.Subject, _ = m["id"].(string)
		c.Email, _ = m["email"].(string)
		if c.Subject != "" {
			c.Groups = stringsClaim(nil, cfg.GroupsClaim)
			return c, nil
		}
	}
	return Claims{}, errors.New("token response carried neither an id_token nor an inline user profile (is the openid scope configured?)")
}

// verifyIDToken checks raw with go-oidc, then the claims it leaves to the caller: azp, iat and the nonce.
func verifyIDToken(ctx context.Context, p *gooidc.Provider, clientID, nonce, raw string) (*gooidc.IDToken, idTokenPayload, error) {
	var payload idTokenPayload
	idt, err := p.Verifier(&gooidc.Config{ClientID: clientID}).Verify(ctx, raw)
	if err != nil {
		return nil, payload, err
	}
	if err := idt.Claims(&payload); err != nil {
		return nil, payload, fmt.Errorf("claims: %w", err)
	}
	if (len(idt.Audience) > 1 || payload.Azp != "") && payload.Azp != clientID {
		return nil, payload, fmt.Errorf("azp %q is not the client id", payload.Azp)
	}
	if idt.IssuedAt.After(time.Now().Add(iatLeeway)) {
		return nil, payload, fmt.Errorf("iat %v is in the future", idt.IssuedAt)
	}
	if idt.Nonce == "" {
		return nil, payload, errors.New("nonce missing")
	}
	if nonce == "" || subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(nonce)) != 1 {
		return nil, payload, errors.New("nonce mismatch")
	}
	return idt, payload, nil
}

// stringsClaim returns the string values of the named claim. Providers render groups as an array of strings; anything else is ignored rather than guessed at.
func stringsClaim(all map[string]any, name string) []string {
	if name == "" {
		return nil
	}
	list, _ := all[name].([]any)
	out := make([]string, 0, len(list))
	for _, v := range list {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
