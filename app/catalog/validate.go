package catalog

import (
	"fmt"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// validateCross runs the few cross-entity rules that *must* hard-fail.
// Soft refs (a Policy listing a disabled RateLimit, a HostKey targeting
// a missing Host, etc.) are no longer rejected here — the per-kind
// sanitizers in build_<kind>.go drop them from the snapshot copy so the
// hot path can never select an unusable resource. The control plane reads
// PG directly and still sees the full attachments.
//
// What stays hard-failing: pricing collisions (two enabled rows competing
// for the same model+host slot are an authoring bug PG can't disambiguate).
// Everything else is soft.
func validateCross(
	_ map[string]struct{}, // providerIDs — unused, kept for callsite stability
	hostIDs map[string]struct{},
	_ []*host.Host,
	_ []*policy.Policy,
	_ []*relaykey.RelayKey,
	enabledModels []*model.Model,
	_ []*hostkey.HostKey,
	_ []*ratelimit.RateLimit,
	enabledPricings []*pricing.Pricing,
	_ []*binding.Binding, // cross-validated via DB UNIQUE + catalogvalidate, not here
) error {
	modelByID := indexBy(enabledModels, func(m *model.Model) string { return m.Meta.ID })

	seenModelHost := map[string]string{} // key → pricing name that claimed it first
	for _, p := range enabledPricings {
		if _, ok := hostIDs[p.Meta.Owner.ID]; !ok {
			continue // sanitizePricing will drop this row from the snapshot
		}
		for _, modelID := range p.Spec.TargetModelIDs {
			if _, ok := modelByID[modelID]; !ok {
				continue
			}
			key := modelID + "|" + p.Meta.Owner.ID
			if first, dup := seenModelHost[key]; dup {
				return fmt.Errorf("duplicate pricing: pricing %q and %q both cover model %q for the same host",
					first, p.Meta.Name, modelID)
			}
			seenModelHost[key] = p.Meta.Name
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
