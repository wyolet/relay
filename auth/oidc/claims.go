package oidc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/oauth2"
)

// Claims is the identity a login produced.
//
// The id_token is decoded without signature verification, and its iss, aud, exp and nonce are not checked: it arrives on the code-exchange response over TLS with client authentication, so the channel, not the signature, is the trust anchor. Bearer tokens presented by callers are a different surface and must be verified.
type Claims struct {
	// Issuer is the configured issuer the login went through, not the token's iss claim.
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
	Sub               string `json:"sub"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Sid               string `json:"sid"`
}

// claimsFromToken reads the caller's identity from the code-exchange response. Standard OIDC carries it as an id_token JWT; some hosted user-management providers instead inline a profile object ({"user": {"id": ..., "email": ...}}). Both are accepted, the JWT first.
func claimsFromToken(tok *oauth2.Token, issuer, groupsClaim string) (Claims, error) {
	if raw, _ := tok.Extra("id_token").(string); raw != "" {
		parts := strings.Split(raw, ".")
		if len(parts) != 3 {
			return Claims{}, errors.New("id_token is not a JWT")
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return Claims{}, fmt.Errorf("id_token payload: %w", err)
		}
		var p idTokenPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return Claims{}, fmt.Errorf("id_token claims: %w", err)
		}
		// The groups claim's name is configuration, so it is read from the generic decode.
		var all map[string]any
		_ = json.Unmarshal(payload, &all)
		if p.Sub == "" {
			return Claims{}, errors.New("id_token has no sub")
		}
		return Claims{
			Issuer:            issuer,
			Subject:           p.Sub,
			Email:             p.Email,
			PreferredUsername: p.PreferredUsername,
			SessionID:         p.Sid,
			Groups:            stringsClaim(all, groupsClaim),
		}, nil
	}
	if m, ok := tok.Extra("user").(map[string]any); ok {
		c := Claims{Issuer: issuer}
		c.Subject, _ = m["id"].(string)
		c.Email, _ = m["email"].(string)
		if c.Subject != "" {
			c.Groups = stringsClaim(nil, groupsClaim)
			return c, nil
		}
	}
	return Claims{}, errors.New("token response carried neither an id_token nor an inline user profile (is the openid scope configured?)")
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
