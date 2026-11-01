package catalog

// resanitizeHostsAfterPolicyChange re-filters every host's policy menu
// against the enabled policies, so a deleted or disabled policy drops out.
func resanitizeHostsAfterPolicyChange(s *Snapshot) {
	for id, h := range s.hostsByID {
		clean := sanitizeHost(h, s.policiesByID)
		s.hostsByID[id] = clean
		s.hostsByName[clean.Meta.Name] = clean
	}
}
