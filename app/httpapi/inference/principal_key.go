package inference

import (
	"net/http"
	"time"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/auth/apikey"
)

// keyPrincipal authenticates a key bearer against the published key plans, writing the 401 and reporting false when no active key has its hash (unknown, disabled, revoked, expired, or a rotated-out hash past its grace).
func keyPrincipal(w http.ResponseWriter, root *plan.Root, bearer string) (*Principal, *key.Key, bool) {
	hash := apikey.Hash(bearer)
	kp := root.Keys.Lookup(hash, time.Now())
	if kp == nil {
		writeAuthErr(w, "invalid api key")
		return nil, nil, false
	}
	return principalOf(kp, hash), kp.Key, true
}

// principalOf is the principal a key plan authenticates; the tenancy comes from its caller plan, never from a per-request lookup.
func principalOf(kp *plan.KeyPlan, hash string) *Principal {
	p := &Principal{
		CredentialKind:     CredentialKey,
		CredentialID:       kp.Key.Meta.ID,
		KeyHash:            hash,
		Key:                kp.Key,
		Caller:             kp.Caller,
		PassthroughAllowed: kp.Key.Spec.PassthroughAllowed,
		PayloadLogging:     kp.Key.Spec.PayloadLoggingEnabled,
		ProjectID:          kp.Caller.Project().ID(),
		TeamID:             kp.Caller.Team().ID(),
	}
	switch kp.Principal.Kind {
	case key.PrincipalServiceAccount:
		p.ServiceAccountID = kp.Principal.ID
	case key.PrincipalUser:
		p.UserID = kp.Principal.ID
	}
	return p
}
