// Helpers shared by the per-kind build_*.go files.
//
// The build pipeline is intentionally one file per resource: each
// build_<kind>.go owns its sanitizer (cross-ref filtering) and its
// "register into the Snapshot" step. build.go orchestrates the order;
// it never reads or writes Snapshot maps directly.
package catalog

// idSet answers "is this id present". A predicate rather than a map so the
// reconcile paths, which run per NOTIFY event, can close over a snapshot map
// directly instead of copying it.
type idSet = func(id string) bool

func setFromIDs[T any](items []T, id func(T) string) idSet {
	set := make(map[string]struct{}, len(items))
	for _, it := range items {
		set[id(it)] = struct{}{}
	}
	return func(id string) bool {
		_, ok := set[id]
		return ok
	}
}

// snapIDs reads membership straight off a snapshot map. Used by reconcile
// Apply paths that sanitize against the current snapshot.
func snapIDs[V any](m map[string]V) idSet {
	return func(id string) bool {
		_, ok := m[id]
		return ok
	}
}

func filterIDs(ids []string, set idSet) []string {
	if len(ids) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if set(id) {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
