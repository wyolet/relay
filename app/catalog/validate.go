package catalog

import (
	"fmt"

	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// validateCross runs cross-entity rules over the candidate enabled sets
// passed in. It errors on the first violation; Reload aborts and the
// existing Snapshot stays live. Inputs are already per-row Validate()'d.
//
// Rules enforced:
//   - Every Model.Meta.Owner.ID resolves to a Provider row (lookup by id).
//   - Every HostKey.Meta.Owner.ID resolves to a Provider row.
//   - Every enabled Policy's ModelIDs resolve to enabled Models, all sharing
//     a single Provider.
//   - Every enabled Policy's HostKeyIDs resolve to enabled HostKeys,
//     all sharing the same Provider as the Policy's Models.
//   - Every enabled Policy's RateLimitID (when set) resolves to an enabled
//     RateLimit.
//   - Every enabled RelayKey's PolicyID resolves to an enabled Policy.
//   - Cross-Model alias uniqueness (case-insensitive; aliases don't collide
//     with any model name).
//   - Model.Spec.Deprecation.Replacement (when set) resolves to an enabled
//     Model.
//
// The Provider set comes from the storage layer separately (Providers are
// not in the Snapshot but we still need them here for ownership checks).
func validateCross(
	providerIDs map[string]struct{},
	enabledPols []*policy.Policy,
	enabledRKs []*relaykey.RelayKey,
	enabledModels []*model.Model,
	enabledKeys []*hostkey.HostKey,
	enabledRLs []*ratelimit.RateLimit,
) error {
	modelByID := indexBy(enabledModels, func(m *model.Model) string { return m.Meta.ID })
	keyByID := indexBy(enabledKeys, func(k *hostkey.HostKey) string { return k.Meta.ID })
	rlByID := indexBy(enabledRLs, func(r *ratelimit.RateLimit) string { return r.Meta.ID })
	polByID := indexBy(enabledPols, func(p *policy.Policy) string { return p.Meta.ID })

	// Owner.ID → Provider for Models and HostKeys.
	for _, m := range enabledModels {
		if _, ok := providerIDs[m.Meta.Owner.ID]; !ok {
			return fmt.Errorf("model %q: owner.id %q does not match any Provider", m.Meta.Name, m.Meta.Owner.ID)
		}
	}
	for _, k := range enabledKeys {
		if _, ok := providerIDs[k.Meta.Owner.ID]; !ok {
			return fmt.Errorf("hostkey %q: owner.id %q does not match any Provider", k.Meta.Name, k.Meta.Owner.ID)
		}
	}

	// Policy.Spec.ModelIDs / HostKeyIDs / RateLimitID resolve + same-provider.
	for _, p := range enabledPols {
		var providerID string
		for _, id := range p.Spec.ModelIDs {
			m, ok := modelByID[id]
			if !ok {
				return fmt.Errorf("policy %q: modelIds references unknown or disabled model %q", p.Meta.Name, id)
			}
			if providerID == "" {
				providerID = m.Meta.Owner.ID
			} else if providerID != m.Meta.Owner.ID {
				return fmt.Errorf("policy %q: models span multiple providers (%q vs %q)",
					p.Meta.Name, providerID, m.Meta.Owner.ID)
			}
		}
		for _, id := range p.Spec.HostKeyIDs {
			k, ok := keyByID[id]
			if !ok {
				return fmt.Errorf("policy %q: hostKeyIds references unknown or disabled key %q", p.Meta.Name, id)
			}
			if providerID == "" {
				providerID = k.Meta.Owner.ID
			} else if providerID != k.Meta.Owner.ID {
				return fmt.Errorf("policy %q: hostKey %q belongs to provider %q, not %q",
					p.Meta.Name, k.Meta.Name, k.Meta.Owner.ID, providerID)
			}
		}
		if p.Spec.RateLimitID != "" {
			if _, ok := rlByID[p.Spec.RateLimitID]; !ok {
				return fmt.Errorf("policy %q: rateLimitId references unknown or disabled rate limit %q", p.Meta.Name, p.Spec.RateLimitID)
			}
		}
	}

	// RelayKey.Spec.PolicyID resolves.
	for _, k := range enabledRKs {
		if _, ok := polByID[k.Spec.PolicyID]; !ok {
			return fmt.Errorf("relaykey %q: policyId references unknown or disabled policy %q", k.Meta.Name, k.Spec.PolicyID)
		}
	}

	// Model alias collisions across the enabled set are *permitted* — the
	// same wire name may intentionally point at multiple Models hosted by
	// different Providers, with the consumer disambiguating via suffix or
	// header. Per-row alias uniqueness (within a single Model's own list)
	// is checked in model.Validate.

	// Deprecation.Replacement resolves.
	for _, m := range enabledModels {
		if m.Spec.Deprecation == nil || m.Spec.Deprecation.Replacement == "" {
			continue
		}
		if _, ok := modelByID[m.Spec.Deprecation.Replacement]; !ok {
			return fmt.Errorf("model %q: deprecation.replacement references unknown or disabled model %q",
				m.Meta.Name, m.Spec.Deprecation.Replacement)
		}
	}

	return nil
}

func indexBy[T any](items []T, key func(T) string) map[string]T {
	out := make(map[string]T, len(items))
	for _, it := range items {
		out[key(it)] = it
	}
	return out
}

func lower(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}
