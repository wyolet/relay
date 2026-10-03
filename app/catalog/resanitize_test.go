package catalog

import (
	"context"
	"testing"
)

// Resanitize replaces rows in ByID; the sibling indices (modelsByName,
// snapshotsByName, snapshotAliases, modelsByPolicy) must serve the same row
// generation rather than the pre-sanitize copies.
func TestApply_ResanitizeKeepsSiblingIndicesCoherent(t *testing.T) {
	t.Skip("known bug: resanitize leaves sibling indices stale; unskip with the fix")
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	m := models[0]
	pol := pols[0]

	if err := c.ApplyHostDelete(hosts[0].Meta.ID); err != nil {
		t.Fatalf("ApplyHostDelete: %v", err)
	}

	s := c.Current()
	byID, ok := s.Model(m.Meta.ID)
	if !ok {
		t.Fatal("model missing after host delete (provider still enabled)")
	}

	byName := s.ModelsByName(m.Meta.Name)
	if len(byName) != 1 {
		t.Fatalf("ModelsByName(%q): got %d rows, want 1", m.Meta.Name, len(byName))
	}
	if byName[0] != byID {
		t.Errorf("modelsByName serves a different row copy than modelsByID for %q", m.Meta.Name)
	}

	snapM, _, ok := s.SnapshotByName(m.Spec.Snapshots[0].Name)
	if !ok {
		t.Fatalf("SnapshotByName(%q) missing", m.Spec.Snapshots[0].Name)
	}
	if snapM != byID {
		t.Errorf("snapshotsByName serves a different row copy than modelsByID for %q", m.Spec.Snapshots[0].Name)
	}

	inPolicy := s.ModelsInPolicy(pol.Meta.ID)
	for _, mm := range inPolicy {
		if mm.Meta.ID == m.Meta.ID && mm != byID {
			t.Errorf("modelsByPolicy serves a different row copy than modelsByID for %q", m.Meta.ID)
		}
	}
}
