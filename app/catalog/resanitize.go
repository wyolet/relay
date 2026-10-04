package catalog

// Re-sanitize dependents when a parent is deleted, so the in-snapshot
// copies stop carrying refs to the now-missing row. The reverse-join maps
// (modelsByPolicy, etc.) are already maintained by the per-kind delete
// functions; this fixes the *spec-level* dangling refs that those joins
// don't touch.

func resanitizePoliciesAfterParentChange(s *Snapshot) {
	models := snapIDs(s.modelsByID)
	keys := snapIDs(s.hostKeysByID)
	rls := snapIDs(s.rateLimitsByID)
	for id, p := range s.policiesByID {
		clean := sanitizePolicy(p, models, keys, rls)
		s.policiesByID[id] = clean
		s.policiesByName[clean.Meta.Name] = clean
		s.unregisterRefs(refKey{Kind: refPolicy, ID: id}, outboundPolicyRefs(p))
		s.registerRefs(refKey{Kind: refPolicy, ID: id}, outboundPolicyRefs(clean))
	}
}

func resanitizeHostsAfterPolicyChange(s *Snapshot) {
	for id, h := range s.hostsByID {
		clean := sanitizeHost(h, s.policiesByID)
		s.hostsByID[id] = clean
		s.hostsByName[clean.Meta.Name] = clean
	}
}
