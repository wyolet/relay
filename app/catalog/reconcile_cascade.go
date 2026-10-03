package catalog

// ── Cascade helpers ───────────────────────────────────────────────────────

// cascadeDelete uses an explicit worklist to avoid deep recursion. For each
// dependent of (kind, id) that fails cross-validation, it is deleted and its
// own dependents pushed onto the worklist.
func cascadeDelete(s *Snapshot, kind refKind, id string) {
	worklist := s.Dependents(kind, id)
	for len(worklist) > 0 {
		dep := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]

		if !rowPresent(s, dep) {
			continue
		}
		// Re-validate the dependent; if it's now invalid, delete it too.
		if !dependentStillValid(s, dep) {
			extra := s.Dependents(dep.Kind, dep.ID)
			worklist = append(worklist, extra...)
			deleteDirect(s, dep)
		}
	}
}

// dependentStillValid returns true only when the row's cross-refs all still
// resolve in s. Pricings stay valid while their owning host and at least one
// target model still resolve, matching full snapshot sanitization.
func dependentStillValid(s *Snapshot, k refKey) bool {
	switch k.Kind {
	case refModel:
		m, ok := s.modelsByID[k.ID]
		if !ok {
			return true // already gone
		}
		return validateModelInSnap(m, s) == nil
	case refHostKey:
		hk, ok := s.hostKeysByID[k.ID]
		if !ok {
			return true
		}
		return validateHostKeyInSnap(hk, s) == nil
	case refPolicy:
		// Disabled rows are checked too: one whose project disappears has to
		// be evicted, or the keys naming it answer 403 against a policy no
		// operator can reach any more.
		p, ok := s.policyLookup(k.ID)
		if !ok {
			return true
		}
		return validatePolicyInSnap(p, s) == nil
	case refPricing:
		p, ok := s.pricingsByID[k.ID]
		if !ok {
			return true
		}
		// For cascade, check refs without duplicate check (we already cleaned
		// pricingByModelHost) — just check host and any model presence.
		if _, ok := s.hostsByID[p.Meta.Owner.ID]; !ok {
			return false
		}
		for _, modelID := range p.Spec.TargetModelIDs {
			if _, ok := s.modelsByID[modelID]; ok {
				return true
			}
		}
		return false
	case refRelayKey:
		rk, ok := s.keysByID[k.ID]
		if !ok {
			return true
		}
		return validateKeyInSnap(rk, s) == nil
	case refBinding:
		b, ok := s.bindingsByID[k.ID]
		if !ok {
			return true
		}
		_, modelOK := s.modelsByID[b.Spec.ModelID]
		_, hostOK := s.hostsByID[b.Spec.HostID]
		return modelOK && hostOK
	case refRateLimit:
		r, ok := s.rateLimitsByID[k.ID]
		if !ok {
			return true
		}
		return validateRateLimitInSnap(r, s) == nil
	case refProject:
		p, ok := s.projectsByID[k.ID]
		if !ok {
			return true
		}
		return validateProjectInSnap(p, s) == nil
	case refServiceAccount:
		sa, ok := s.serviceAccountsByID[k.ID]
		if !ok {
			return true
		}
		return validateServiceAccountInSnap(sa, s) == nil
	case refRoleBinding:
		b, ok := s.roleBindingsByID[k.ID]
		if !ok {
			return true
		}
		return validateRoleBindingInSnap(b, s) == nil
	case refPolicyBinding:
		b, ok := s.policyBindingsByID[k.ID]
		if !ok {
			return true
		}
		return validatePolicyBindingInSnap(b, s) == nil
	}
	return true
}

// rowPresent is the non-test equivalent of the test helper rowExists.
func rowPresent(s *Snapshot, k refKey) bool {
	switch k.Kind {
	case refProvider:
		_, ok := s.providersByID[k.ID]
		return ok
	case refHost:
		_, ok := s.hostsByID[k.ID]
		return ok
	case refModel:
		_, ok := s.modelsByID[k.ID]
		return ok
	case refHostKey:
		_, ok := s.hostKeysByID[k.ID]
		return ok
	case refRateLimit:
		_, ok := s.rateLimitsByID[k.ID]
		return ok
	case refPolicy:
		// Disabled counts as present: the row is in the snapshot, just out of
		// the routing indices, so re-enabling it is a patch, not a recovery.
		return s.policyResolvable(k.ID)
	case refPricing:
		_, ok := s.pricingsByID[k.ID]
		return ok
	case refRelayKey:
		_, ok := s.keysByID[k.ID]
		return ok
	case refBinding:
		_, ok := s.bindingsByID[k.ID]
		return ok
	case refTeam:
		_, ok := s.teamsByID[k.ID]
		return ok
	case refProject:
		_, ok := s.projectsByID[k.ID]
		return ok
	case refServiceAccount:
		_, ok := s.serviceAccountsByID[k.ID]
		return ok
	case refRole:
		_, ok := s.rolesByID[k.ID]
		return ok
	case refRoleBinding:
		_, ok := s.roleBindingsByID[k.ID]
		return ok
	case refPolicyBinding:
		_, ok := s.policyBindingsByID[k.ID]
		return ok
	}
	return false
}

// deleteDirect calls the appropriate delete helper for (kind, id).
func deleteDirect(s *Snapshot, k refKey) {
	switch k.Kind {
	case refModel:
		deleteModel(s, k.ID)
	case refHostKey:
		deleteHostKey(s, k.ID)
	case refPolicy:
		deletePolicy(s, k.ID)
	case refPricing:
		deletePricing(s, k.ID)
	case refRelayKey:
		deleteKey(s, k.ID)
	case refRateLimit:
		deleteRateLimit(s, k.ID)
	case refProvider:
		deleteProvider(s, k.ID)
	case refHost:
		deleteHost(s, k.ID)
	case refBinding:
		deleteBinding(s, k.ID)
	case refTeam:
		deleteTeam(s, k.ID)
	case refProject:
		deleteProject(s, k.ID)
	case refServiceAccount:
		deleteServiceAccount(s, k.ID)
	case refRole:
		deleteRole(s, k.ID)
	case refRoleBinding:
		deleteRoleBinding(s, k.ID)
	case refPolicyBinding:
		deletePolicyBinding(s, k.ID)
	}
}
