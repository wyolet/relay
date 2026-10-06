package inference

import (
	"errors"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/serviceaccount"
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
	Subjects                     []string
	UserID, ServiceAccountID     string // exactly one set
	ProjectID, TeamID            string // empty for personal keys
	CredentialKind, CredentialID string // CredentialKey/CredentialToken; key id or jti
	KeyHash                      string // hash presented; empty for tokens
	Key                          *key.Key
	ServiceAccount               *serviceaccount.ServiceAccount
	Policy                       *policy.Policy // nil → policy-less
	PassthroughAllowed           bool
	PayloadLogging               bool // the credential's own flag; read through CapturesPayload

	// TokenExp/TokenVer are the claims a token credential was admitted
	// under, kept so a long-lived connection can re-check them without the
	// bearer. Zero for a key.
	TokenExp int64
	TokenVer int

	// token is the verified claims a token credential was admitted with, so
	// a WebSocket frame can re-derive the subjects without the bearer.
	token *cacheEntry
}

// Recheck re-validates an already-admitted principal against the current
// snapshot. The HTTP path resolves once per request and never needs it; a
// WebSocket admits on the upgrade and then serves frames for hours, so
// every frame re-runs the revocation checks the upgrade made. Snapshot
// reads only — no signature work, no Postgres.
func (p *Principal) Recheck(snap *appcatalog.Snapshot, now time.Time) error {
	if p == nil || snap == nil {
		return nil
	}
	// A credential scoped to a project stops working when the project (or
	// the team above it) leaves the snapshot: its limits and attribution
	// no longer exist.
	if p.ProjectID != "" {
		if _, ok := snap.Project(p.ProjectID); !ok {
			return errors.New("project unavailable")
		}
	}
	if p.CredentialKind == CredentialToken {
		if p.TokenExp > 0 && p.TokenExp <= now.Unix() {
			return errors.New("token expired")
		}
		if ver, ok := snap.TokenVersion(p.UserID); !ok || ver != p.TokenVer {
			return errors.New(msgTokenRevoked)
		}
		return nil
	}
	k, matchedPrevious := snap.KeyByHash(p.KeyHash)
	switch {
	case k == nil:
		return errors.New("invalid api key")
	case !k.IsEnabled():
		return errors.New("api key disabled")
	case k.Spec.RevokedAt != nil:
		return errors.New("api key revoked")
	case k.Spec.ExpiresAt != nil && !now.Before(*k.Spec.ExpiresAt):
		return errors.New("api key expired")
	case matchedPrevious && !k.InGrace(now):
		return errors.New("api key rotated")
	case !keyUserEnabled(snap, k):
		return errors.New("api key disabled")
	}
	return nil
}

// keyUserEnabled reports whether a personal key's user may still act: a
// personal key is that user's credential, so disabling the user stops it.
// A key with no user id (issued by the admin token) has no user to check.
func keyUserEnabled(snap *appcatalog.Snapshot, k *key.Key) bool {
	if k.Spec.Principal.Kind != key.PrincipalUser || k.Spec.Principal.ID == "" {
		return true
	}
	return snap.UserEnabled(k.Spec.Principal.ID)
}

// CapturesPayload reports whether this principal's requests have their
// bodies captured, the same answer on every runner. Nil (anonymous proxy
// traffic) carries no opt-in and captures nothing.
func (p *Principal) CapturesPayload() bool {
	if p == nil {
		return false
	}
	return policy.CapturesPayload(p.Policy, p.PayloadLogging)
}

// PolicyID returns the resolved policy's id, or "" for the policy-less
// flow. Nil-safe so error paths can call it unguarded.
func (p *Principal) PolicyID() string {
	if p == nil || p.Policy == nil {
		return ""
	}
	return p.Policy.Meta.ID
}
