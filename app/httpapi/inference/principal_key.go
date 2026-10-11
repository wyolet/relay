package inference

import (
	"net/http"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/apikey"
)

// keyPrincipal authenticates a key bearer, writing the 401 and reporting
// false when the key is unknown or no longer usable.
func keyPrincipal(w http.ResponseWriter, snap *appcatalog.Snapshot, bearer string) (*Principal, *key.Key, bool) {
	hash := apikey.Hash(bearer)
	k, matchedPrevious := snap.KeyByHash(hash)
	if k == nil {
		writeAuthErr(w, "invalid api key")
		return nil, nil, false
	}
	now := time.Now()
	switch {
	case !k.IsEnabled():
		writeAuthErr(w, "api key disabled")
		return nil, nil, false
	case k.Spec.RevokedAt != nil:
		writeAuthErr(w, "api key revoked")
		return nil, nil, false
	case k.Spec.ExpiresAt != nil && !now.Before(*k.Spec.ExpiresAt):
		writeAuthErr(w, "api key expired")
		return nil, nil, false
	case matchedPrevious && !k.InGrace(now):
		writeAuthErr(w, "api key rotated")
		return nil, nil, false
	case !keyUserEnabled(snap, k):
		writeAuthErr(w, "api key disabled")
		return nil, nil, false
	}
	return buildPrincipal(snap, k, hash), k, true
}

// buildPrincipal resolves the key's identity and tenancy from the snapshot.
// Subjects is the precomputed list, taken by slice header — never rebuilt
// per request.
func buildPrincipal(snap *appcatalog.Snapshot, k *key.Key, hash string) *Principal {
	p := &Principal{
		Subjects:           snap.SubjectsForKey(k.Meta.ID),
		CredentialKind:     CredentialKey,
		CredentialID:       k.Meta.ID,
		KeyHash:            hash,
		Key:                k,
		PassthroughAllowed: k.Spec.PassthroughAllowed,
		PayloadLogging:     k.Spec.PayloadLoggingEnabled,
	}
	if k.Spec.PolicyID != "" {
		if pol, ok := policyOrDisabled(snap, k.Spec.PolicyID); ok {
			p.Policy = pol
		}
	}
	switch k.Spec.Principal.Kind {
	case key.PrincipalServiceAccount:
		p.ServiceAccountID = k.Spec.Principal.ID
		if sa, ok := snap.ServiceAccount(k.Spec.Principal.ID); ok {
			p.ServiceAccount = sa
			p.ProjectID = sa.Spec.ProjectID
			if proj, ok := snap.Project(sa.Spec.ProjectID); ok {
				p.TeamID = proj.Spec.TeamID
			}
		}
	case key.PrincipalUser:
		p.UserID = k.Spec.Principal.ID
		// A personal key on a project-owned policy spends that project's
		// upstream credentials (its owner was allowed to point it there), so
		// the request carries the project's attribution and limits.
		if p.Policy != nil && p.Policy.Meta.Owner.Kind == meta.OwnerProject {
			p.ProjectID = p.Policy.Meta.Owner.ID
			if proj, ok := snap.Project(p.ProjectID); ok {
				p.TeamID = proj.Spec.TeamID
			}
		}
	}
	return p
}
