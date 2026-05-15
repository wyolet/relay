// Helpers shared by the per-kind build_*.go files.
//
// The build pipeline is intentionally one file per resource: each
// build_<kind>.go owns its sanitizer (cross-ref filtering) and its
// "register into the Snapshot" step. build.go orchestrates the order;
// it never reads or writes Snapshot maps directly.
package catalog

type idSet = map[string]struct{}

func setFromIDs[T any](items []T, id func(T) string) idSet {
	out := make(idSet, len(items))
	for _, it := range items {
		out[id(it)] = struct{}{}
	}
	return out
}

// snapIDs returns the id set from a snapshot map. Used by reconcile Apply
// paths that sanitize against the current snapshot.
func snapIDs[V any](m map[string]V) idSet {
	out := make(idSet, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out
}

func filterIDs(ids []string, set idSet) []string {
	if len(ids) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := set[id]; ok {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
