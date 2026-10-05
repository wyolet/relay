package inference

import (
	"context"
	"net/http"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
)

// resolvePolicy fills in the Policy a principal doesn't already carry from
// its credential: the service account's override first, then the project's
// policy bindings in (priority, name) order. Reports false — after writing
// the response — when nothing resolves and the caller is not eligible for
// the policy-less flow.
func resolvePolicy(w http.ResponseWriter, snap *appcatalog.Snapshot, p *Principal) bool {
	var bindings []*policybinding.PolicyBinding
	p.Policy, bindings = governingPolicy(snap, p)
	if p.Policy != nil {
		// A resolved-but-disabled policy is an answer, not a miss: falling
		// through to a broader binding, or to the policy-less flow, would
		// hand the caller more than the operator left switched on.
		if !p.Policy.IsEnabled() {
			writeForbidden(w, "policy_disabled", "policy is disabled")
			return false
		}
		return true
	}
	// A personal key with no project falls through to the policy-less flow,
	// which routing gates on settings.Inference.AllowMissingPolicy. Anything
	// scoped to a project — a token always is — must resolve a policy.
	if p.CredentialKind == CredentialKey && p.ProjectID == "" {
		return true
	}
	// Keys the tenancy migration parked in the legacy project had no policy
	// before it and keep that behaviour until an operator binds one there.
	if p.CredentialKind == CredentialKey && p.ProjectID == project.LegacyID && len(bindings) == 0 {
		return true
	}
	writeForbidden(w, "no_policy", "no policy is bound to this principal")
	return false
}

// governingPolicy returns the policy that governs p: the one its credential carries, else the service account's override, else the first of the project's policy bindings, in (priority, name) order, that names one of p's subjects. A disabled policy is returned like an enabled one. bindings is the project's list when it was consulted.
func governingPolicy(snap *appcatalog.Snapshot, p *Principal) (pol *policy.Policy, bindings []*policybinding.PolicyBinding) {
	if p.Policy != nil {
		return p.Policy, nil
	}
	if p.ServiceAccount != nil && p.ServiceAccount.Spec.PolicyID != "" {
		if pol, ok := policyOrDisabled(snap, p.ServiceAccount.Spec.PolicyID); ok {
			return pol, nil
		}
	}
	if p.ProjectID == "" {
		return nil, nil
	}
	bindings = snap.PolicyBindingsForProject(p.ProjectID)
	for _, b := range bindings {
		if !bindingMatches(b, p.Subjects) {
			continue
		}
		if pol, ok := policyOrDisabled(snap, b.Spec.PolicyID); ok {
			return pol, bindings
		}
	}
	return nil, bindings
}

// GoverningPolicy returns the policy that governs the request's authenticated principal, resolved in the order inference resolves it, or nil when none does. It rejects nothing and writes no response, for endpoints behind AuthenticateMiddleware that accept a credential without a policy and still honour one that has it. A disabled policy is returned: it still governs.
func GoverningPolicy(ctx context.Context) *policy.Policy {
	p, snap := PrincipalFrom(ctx), SnapshotFrom(ctx)
	if p == nil || snap == nil {
		return nil
	}
	pol, _ := governingPolicy(snap, p)
	return pol
}

// policyOrDisabled resolves a policy id, falling back to the disabled row so
// a credential pointing at a switched-off policy is answered rather than
// treated as pointing at nothing.
func policyOrDisabled(snap *appcatalog.Snapshot, id string) (*policy.Policy, bool) {
	if pol, ok := snap.Policy(id); ok {
		return pol, true
	}
	return snap.DisabledPolicy(id)
}

// bindingMatches reports whether the binding names a subject the principal
// carries. Both lists hold a handful of entries, so the nested scan beats
// building a set per request.
func bindingMatches(b *policybinding.PolicyBinding, subjects []string) bool {
	for _, want := range b.SubjectKeys {
		for _, have := range subjects {
			if have == want {
				return true
			}
		}
	}
	return false
}
