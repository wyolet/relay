package inference

import (
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/pkg/crypto"
)

// Credential kinds a Principal can be authenticated by.
const (
	CredentialKey   = "key"
	CredentialToken = "token"
)

// Principal is who the request is acting as, resolved once at the edge so
// nothing downstream re-derives it. Credential-specific fields
// (CredentialKind, CredentialID) name the credential presented, not the
// subject.
type Principal struct {
	UserID, ServiceAccountID     string // exactly one set
	ProjectID, TeamID            string // empty for a user's own key
	CredentialKind, CredentialID string // CredentialKey/CredentialToken; key id or jti
	KeyHash                      string // hash presented; empty for tokens
	Key                          *key.Key
	// Caller is the principal's folded caller ladder: what it may reach and the counters it must fit.
	Caller             *plan.CallerPlan
	PassthroughAllowed bool
	PayloadLogging     bool // the credential's own flag; read through CapturesPayload

	// TokenExp/TokenVer are the claims a token credential was admitted
	// under, kept so a long-lived connection can re-check them without the
	// bearer. Zero for a key.
	TokenExp int64
	TokenVer int

	// claims and claimsTyp are what a token credential verified to, so a WebSocket frame re-checks them without the bearer.
	claims    crypto.TokenClaims
	claimsTyp string
}

// CapturesPayload reports whether this principal's requests have their
// bodies captured, the same answer on every runner: the credential's own
// opt-in, until capture becomes a scope setting. Nil (anonymous proxy
// traffic) carries no opt-in and captures nothing.
func (p *Principal) CapturesPayload() bool {
	return p != nil && p.PayloadLogging
}

// Policy returns the innermost caller policy, nil when no caller rung binds one. Nil-safe so error paths can call it unguarded.
func (p *Principal) Policy() *policy.Policy {
	if p == nil || p.Caller == nil {
		return nil
	}
	return p.Caller.InnermostPolicy()
}

// PolicyID returns the innermost caller policy's id, or "".
func (p *Principal) PolicyID() string {
	if pol := p.Policy(); pol != nil {
		return pol.Meta.ID
	}
	return ""
}
