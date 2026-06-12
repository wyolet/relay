package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/pkg/slug"
)

type ovlList []*overlay.Overlay

func (l ovlList) List(context.Context) ([]*overlay.Overlay, error) { return l, nil }

func fixtureModelID(t *testing.T, c *Catalog, name string) string {
	t.Helper()
	ms := c.Current().ModelsByName(name)
	if len(ms) != 1 {
		t.Fatalf("fixture model %q: got %d", name, len(ms))
	}
	return ms[0].Meta.ID
}

func aliasOverlay(id string) *overlay.Overlay {
	return &overlay.Overlay{
		Kind:       overlay.KindModel,
		ResourceID: id,
		Patch:      []byte(`{"aliases":["my-custom-nick"],"family":"custom"}`),
	}
}

// resolvesAlias reports whether the snapshot resolves the overlay-added
// alias to the given model id.
func resolvesAlias(s *Snapshot, ref, modelID string) bool {
	r, ok := s.ResolveAlias(slug.From(ref), true)
	return ok && r.Model.Meta.ID == modelID
}

func TestOverlay_AppliedAtBuild(t *testing.T) {
	c := catalogFromFixture(t)
	id := fixtureModelID(t, c, "gpt-4o")
	c.UseOverlays(ovlList{aliasOverlay(id)})
	if err := c.Reload(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()

	eff, _ := s.Model(id)
	if eff.Spec.Family != "custom" || len(eff.Spec.Aliases) != 1 {
		t.Fatalf("effective not merged: %+v", eff.Spec)
	}
	if !resolvesAlias(s, "my-custom-nick", id) {
		t.Error("overlay alias not indexed at build")
	}
	// Template stashed pristine.
	tmpl := s.templateModel(id)
	if tmpl == eff || tmpl.Spec.Family != "" || len(tmpl.Spec.Aliases) != 0 {
		t.Errorf("template not pristine: %+v", tmpl.Spec)
	}
}

func TestOverlay_ReconcileUpsertAndDelete(t *testing.T) {
	c := catalogFromFixture(t)
	id := fixtureModelID(t, c, "gpt-4o")

	if err := c.ApplyOverlayUpsert(aliasOverlay(id)); err != nil {
		t.Fatalf("ApplyOverlayUpsert: %v", err)
	}
	s := c.Current()
	if eff, _ := s.Model(id); eff.Spec.Family != "custom" {
		t.Fatal("effective not live after overlay upsert")
	}
	if !resolvesAlias(s, "my-custom-nick", id) {
		t.Fatal("overlay alias not resolvable after upsert")
	}

	// Factory reset: template restored, alias gone, stash cleared.
	if err := c.ApplyOverlayDelete(overlay.KindModel, id); err != nil {
		t.Fatalf("ApplyOverlayDelete: %v", err)
	}
	s = c.Current()
	if eff, _ := s.Model(id); eff.Spec.Family != "" || len(eff.Spec.Aliases) != 0 {
		t.Errorf("template not restored: %+v", eff.Spec)
	}
	if resolvesAlias(s, "my-custom-nick", id) {
		t.Error("overlay alias survived factory reset")
	}
	if _, stashed := s.modelTemplates[id]; stashed {
		t.Error("template stash not cleared")
	}
}

// TestOverlay_SurvivesTemplateReseed is the feature's reason to exist:
// a template update (catalog re-seed / upstream edit) flows through
// while the overlay's fields stay pinned.
func TestOverlay_SurvivesTemplateReseed(t *testing.T) {
	c := catalogFromFixture(t)
	id := fixtureModelID(t, c, "gpt-4o")
	if err := c.ApplyOverlayUpsert(aliasOverlay(id)); err != nil {
		t.Fatal(err)
	}

	// Simulate the re-seed: a fresh template row with upstream changes
	// (new version + a catalog-shipped alias).
	tmpl := *c.Current().templateModel(id)
	tmpl.Spec.Version = "v2"
	tmpl.Spec.Aliases = []string{"catalog-shipped[1m]"}
	if err := c.ApplyModelUpsert(&tmpl); err != nil {
		t.Fatalf("template reseed: %v", err)
	}

	s := c.Current()
	eff, _ := s.Model(id)
	if eff.Spec.Version != "v2" {
		t.Error("un-overridden template change did not flow through")
	}
	if eff.Spec.Family != "custom" {
		t.Error("overlay field clobbered by reseed")
	}
	// Union: catalog-shipped alias AND the user's both resolve.
	if !resolvesAlias(s, "catalog-shipped[1m]", id) {
		t.Error("template alias lost")
	}
	if !resolvesAlias(s, "my-custom-nick", id) {
		t.Error("user alias lost on reseed")
	}
}

func TestOverlay_QuarantineServesTemplate(t *testing.T) {
	c := catalogFromFixture(t)
	id := fixtureModelID(t, c, "gpt-4o")
	bad := &overlay.Overlay{
		Kind:       overlay.KindModel,
		ResourceID: id,
		Patch:      []byte(`{"pointer":"no-such-snapshot"}`), // invalid effective
	}
	if err := c.ApplyOverlayUpsert(bad); err != nil {
		t.Fatalf("upsert (quarantine is not a write error): %v", err)
	}
	s := c.Current()
	eff, ok := s.Model(id)
	if !ok {
		t.Fatal("model vanished under quarantined overlay")
	}
	if eff.Spec.Pointer == "no-such-snapshot" {
		t.Fatal("invalid merge served instead of pristine template")
	}
	// Still routable by its real name.
	if _, _, _, ok := s.ResolveSnapshot(slug.From("gpt-4o-2025-01-01")); !ok {
		t.Error("template no longer resolvable under quarantine")
	}
}

func TestOverlay_InertWhenTargetMissing(t *testing.T) {
	c := catalogFromFixture(t)
	orphan := &overlay.Overlay{
		Kind:       overlay.KindModel,
		ResourceID: "no-such-model",
		Patch:      []byte(`{"aliases":["ghost"]}`),
	}
	if err := c.ApplyOverlayUpsert(orphan); err != nil {
		t.Fatalf("orphan overlay should register inert: %v", err)
	}
	if resolvesAlias(c.Current(), "ghost", "no-such-model") {
		t.Error("orphan overlay produced a resolvable alias")
	}
	// Model deletion keeps the overlay registered but clears the stash.
	id := fixtureModelID(t, c, "gpt-4o")
	if err := c.ApplyOverlayUpsert(aliasOverlay(id)); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyModelDelete(id); err != nil {
		t.Fatal(err)
	}
	s := c.Current()
	if _, stashed := s.modelTemplates[id]; stashed {
		t.Error("template stash survived model delete")
	}
	if resolvesAlias(s, "my-custom-nick", id) {
		t.Error("overlay alias survived model delete")
	}
}

func TestOverlay_CloneIsolation(t *testing.T) {
	c := catalogFromFixture(t)
	id := fixtureModelID(t, c, "gpt-4o")
	if err := c.ApplyOverlayUpsert(aliasOverlay(id)); err != nil {
		t.Fatal(err)
	}
	before := c.Current()
	if err := c.ApplyOverlayDelete(overlay.KindModel, id); err != nil {
		t.Fatal(err)
	}
	// The captured snapshot still serves the overlaid state.
	if eff, _ := before.Model(id); eff.Spec.Family != "custom" {
		t.Error("old snapshot lost overlay state after reconcile")
	}
	if !resolvesAlias(before, "my-custom-nick", id) {
		t.Error("old snapshot lost overlay alias after reconcile")
	}
}
