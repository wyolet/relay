package catalog

// clone returns a shallow copy of s: every map header is duplicated so the
// reconciler can mutate the clone without affecting the live snapshot. Rows
// themselves are immutable and shared.
//
// Slice-valued maps (modelsByName, modelsByPolicy, hostKeysByPolicy) also have
// their slices copied so append doesn't bleed back.
//
// The refsBy* maps carry a second level of copying: the outer map is new and
// each inner refSet is also copied, because the reconciler mutates them.
func (s *Snapshot) clone() *Snapshot {
	c := &Snapshot{
		providersByID:   shallowMap(s.providersByID),
		providersByName: shallowMap(s.providersByName),

		hostsByID:   shallowMap(s.hostsByID),
		hostsByName: shallowMap(s.hostsByName),

		policiesByID:   shallowMap(s.policiesByID),
		policiesByName: shallowMap(s.policiesByName),

		modelsByID:      shallowMap(s.modelsByID),
		modelsByName:    copySliceMap(s.modelsByName),
		snapshotsByName: shallowMap(s.snapshotsByName),
		snapshotAliases: shallowMap(s.snapshotAliases),

		hostKeysByID:     shallowMap(s.hostKeysByID),
		rateLimitsByID:   shallowMap(s.rateLimitsByID),
		rateLimitsByName: shallowMap(s.rateLimitsByName),

		relayKeysByID:   shallowMap(s.relayKeysByID),
		relayKeysByHash: shallowMap(s.relayKeysByHash),

		modelsByPolicy:        copySliceMap(s.modelsByPolicy),
		hostKeysByPolicy:      copySliceMap(s.hostKeysByPolicy),
		rateLimitByPolicy:     shallowMap(s.rateLimitByPolicy),
		allowedCombosByPolicy: shallowMap(s.allowedCombosByPolicy),

		pricingsByID:       shallowMap(s.pricingsByID),
		pricingByModelHost: shallowMap(s.pricingByModelHost),

		refsByProvider:  copyRefMap(s.refsByProvider),
		refsByHost:      copyRefMap(s.refsByHost),
		refsByModel:     copyRefMap(s.refsByModel),
		refsByHostKey:   copyRefMap(s.refsByHostKey),
		refsByRateLimit: copyRefMap(s.refsByRateLimit),
		refsByPolicy:    copyRefMap(s.refsByPolicy),
	}
	return c
}

func shallowMap[K comparable, V any](m map[K]V) map[K]V {
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copySliceMap[K comparable, V any](m map[K][]V) map[K][]V {
	out := make(map[K][]V, len(m))
	for k, sl := range m {
		cp := make([]V, len(sl))
		copy(cp, sl)
		out[k] = cp
	}
	return out
}

func copyRefMap(m map[string]refSet) map[string]refSet {
	out := make(map[string]refSet, len(m))
	for k, s := range m {
		ns := make(refSet, len(s))
		for rk := range s {
			ns[rk] = struct{}{}
		}
		out[k] = ns
	}
	return out
}
