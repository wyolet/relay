package token

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Audience is the `aud` claim, which RFC 7519 lets be one string or an array
// of them. A single audience encodes as a string.
type Audience []string

// UnmarshalJSON accepts a string, an array of strings, or null.
func (a *Audience) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) {
		*a = nil
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var one string
		if err := json.Unmarshal(b, &one); err != nil {
			return err
		}
		*a = Audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

// MarshalJSON writes one audience as a string and any other count as an array.
func (a Audience) MarshalJSON() ([]byte, error) {
	if len(a) == 1 {
		return json.Marshal(a[0])
	}
	return json.Marshal([]string(a))
}

// Contains reports whether aud is one of the audiences.
func (a Audience) Contains(aud string) bool {
	for _, v := range a {
		if v == aud {
			return true
		}
	}
	return false
}

// Claims are the registered claims Validate checks. A new claim set can embed
// it; one with its own field names copies the values in.
type Claims struct {
	Issuer    string   `json:"iss,omitempty"`
	Subject   string   `json:"sub,omitempty"`
	Audience  Audience `json:"aud,omitempty"`
	Expiry    int64    `json:"exp,omitempty"`
	NotBefore int64    `json:"nbf,omitempty"`
	IssuedAt  int64    `json:"iat,omitempty"`
	ID        string   `json:"jti,omitempty"`
	// Type is the header's `typ`, not a claim: set it from HeaderType when
	// Want.Type is set.
	Type string `json:"-"`
}

// Want is what Validate requires of a token.
type Want struct {
	// Issuer, when set, must equal the token's `iss`.
	Issuer string
	// Audience, when set, must be one of the token's audiences. When empty, a
	// token that names any audience is refused: it was issued for someone else.
	Audience string
	// Type, when set, must match the header's `typ` (case-insensitive).
	Type string
	// Leeway is the clock skew tolerated on exp, nbf and iat. Negative is 0.
	Leeway time.Duration
}

// Validate's errors, one per check. ErrExpired also covers a missing exp.
var (
	ErrType           = errors.New("token: unexpected typ")
	ErrIssuer         = errors.New("token: unexpected issuer")
	ErrAudience       = errors.New("token: audience does not match")
	ErrExpired        = errors.New("token: expired")
	ErrNotYetValid    = errors.New("token: not valid yet")
	ErrIssuedInFuture = errors.New("token: issued in the future")
)

// Validate checks c against want at now, in a fixed order: typ, issuer,
// audience, then exp (required), nbf and iat (each checked when non-zero).
// The first failing check is returned. Signature verification is Parse's.
func Validate(c Claims, now time.Time, want Want) error {
	leeway := max(want.Leeway, 0)
	switch {
	case want.Type != "" && !strings.EqualFold(c.Type, want.Type):
		return ErrType
	case want.Issuer != "" && c.Issuer != want.Issuer:
		return ErrIssuer
	case want.Audience == "" && len(c.Audience) > 0,
		want.Audience != "" && !c.Audience.Contains(want.Audience):
		return ErrAudience
	case c.Expiry == 0 || !now.Before(time.Unix(c.Expiry, 0).Add(leeway)):
		return ErrExpired
	case c.NotBefore != 0 && now.Add(leeway).Before(time.Unix(c.NotBefore, 0)):
		return ErrNotYetValid
	case c.IssuedAt != 0 && time.Unix(c.IssuedAt, 0).After(now.Add(leeway)):
		return ErrIssuedInFuture
	}
	return nil
}
