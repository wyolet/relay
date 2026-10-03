package inference

import (
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
	if p.Policy == nil && p.ServiceAccount != nil && p.ServiceAccount.Spec.PolicyID != "" {
		if pol, ok := policyOrDisabled(snap, p.ServiceAccount.Spec.PolicyID); ok {
			p.Policy = pol
		}
	}
	var bindings []*policybinding.PolicyBinding
	if p.Policy == nil && p.ProjectID != "" {
		bindings = snap.PolicyBindingsForProject(p.ProjectID)
		for _, b := range bindings {
			if !bindingMatches(b, p.Subjects) {
				continue
			}
			if pol, ok := policyOrDisabled(snap, b.Spec.PolicyID); ok {
				p.Policy = pol
				break
			}
		}
	}
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
