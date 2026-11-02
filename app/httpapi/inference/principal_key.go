package inference

import (
	"net/http"
	"time"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/auth/apikey"
)

// keyPrincipal authenticates a key bearer against the published key plans, writing the 401 and reporting false when no active key has its hash (unknown, disabled, revoked, expired, or a rotated-out hash past its grace), and the 403 when lane names an account the key may not run in.
func keyPrincipal(w http.ResponseWriter, root *plan.Root, bearer, lane string) (*Principal, *key.Key, bool) {
	hash := apikey.Hash(bearer)
	kp := root.Keys.Lookup(hash, time.Now())
	if kp == nil {
		writeAuthErr(w, "invalid api key")
		return nil, nil, false
	}
	p, refused := keyPrincipalIn(root, kp, hash, lane)
	if refused != nil {
		writeForbidden(w, refused.Code, refused.Error())
		return nil, nil, false
	}
	return p, kp.Key, true
}

// keyPrincipalIn is the principal kp authenticates, acting in lane; account_not_held when the key may not run there.
func keyPrincipalIn(root *plan.Root, kp *plan.KeyPlan, hash, lane string) (*Principal, *plan.Refusal) {
	if lane == "" {
		return principalOf(kp, hash), nil
	}
	cp, refused := root.LaneFor(kp, lane)
	if refused != nil {
		return nil, refused
	}
	p := principalIn(kp, hash, cp)
	p.SwitchedLaneID = cp.Lane.Meta.ID
	return p, nil
}

// principalOf is the principal a key plan authenticates, in the key's default lane.
func principalOf(kp *plan.KeyPlan, hash string) *Principal { return principalIn(kp, hash, kp.Caller) }

// principalIn is the principal a key plan authenticates, acting in lane cp; the tenancy comes from the caller plan, never from a per-request lookup.
func principalIn(kp *plan.KeyPlan, hash string, cp *plan.CallerPlan) *Principal {
	p := &Principal{
		CredentialKind:     CredentialKey,
		CredentialID:       kp.Key.Meta.ID,
		KeyHash:            hash,
		Key:                kp.Key,
		Caller:             cp,
		PassthroughAllowed: kp.Key.Spec.PassthroughAllowed,
		PayloadLogging:     kp.Key.Spec.PayloadLoggingEnabled,
		ProjectID:          cp.Project().ID(),
		TeamID:             cp.Team().ID(),
	}
	switch kp.Principal.Kind {
	case key.PrincipalServiceAccount:
		p.ServiceAccountID = kp.Principal.ID
	case key.PrincipalUser:
		p.UserID = kp.Principal.ID
	}
	return p
}
