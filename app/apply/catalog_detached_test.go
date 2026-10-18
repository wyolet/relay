package apply

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settings/settingstest"
)

// detachedModel is a provider-owned catalog row a catalog upgrade dropped.
func detachedModel(providerID string, at time.Time) *model.Model {
	return &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o", DisplayName: "GPT-4o",
			Owner:             meta.Owner{Kind: meta.OwnerProvider, ID: providerID},
			CatalogDetachedAt: &at,
		},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "gpt-4o"}}, Pointer: "gpt-4o"},
	}
}

func modelDoc(providerID, displayName string) *manifest.ModelDTO {
	d := &manifest.ModelDTO{APIVersion: manifest.APIVersion, Kind: "Model"}
	d.Metadata.Name = "gpt-4o"
	d.Metadata.DisplayName = displayName
	d.Metadata.Owner.Kind = meta.OwnerProvider
	d.Metadata.Owner.ID = providerID
	d.Spec.Snapshots = []model.Snapshot{{Name: "gpt-4o"}}
	d.Spec.Pointer = "gpt-4o"
	return d
}

func planModel(t *testing.T, b *builder, stored *model.Model, doc *manifest.ModelDTO) ([]*model.Model, error) {
	t.Helper()
	written, err := planOne(context.Background(), b, kindWiring[manifest.ModelDTO, model.Model]{
		Kind: "Model", Docs: []*manifest.ModelDTO{doc},
		Names: map[string]string{stored.Meta.Name: stored.Meta.ID}, Rows: b.rows.Models,
		To:   manifest.ToModel,
		Meta: func(x *model.Model) *meta.Metadata { return &x.Meta },
	})
	if err == nil {
		runWrites(t, b)
	}
	return *written, err
}

// An operator applying a detached row's document neither re-attaches it nor
// sees the marker as drift: only the catalog reseed moves it.
func TestApplyKeepsCatalogDetachedAtOutOfTheDiff(t *testing.T) {
	providerID := meta.NewID()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	stored := detachedModel(providerID, at)

	b := &builder{rows: &Rows{Models: []*model.Model{stored}}, lic: license.Community}
	written, err := planModel(t, b, stored, modelDoc(providerID, "GPT-4o"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(b.entries) != 1 || b.entries[0].Action != ActionUnchanged || len(written) != 0 {
		t.Fatalf("entries = %+v, written %d; want one unchanged row and no write", b.entries, len(written))
	}

	b = &builder{rows: &Rows{Models: []*model.Model{stored}}, lic: license.Community}
	written, err = planModel(t, b, stored, modelDoc(providerID, "GPT-4o (renamed)"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(b.entries) != 1 || b.entries[0].Action != ActionUpdate || len(written) != 1 {
		t.Fatalf("entries = %+v, written %d; want one update", b.entries, len(written))
	}
	for _, f := range b.entries[0].ChangedFields {
		if f == "metadata.catalogDetachedAt" {
			t.Fatalf("the marker reached the diff: %v", b.entries[0].ChangedFields)
		}
	}
	if got := written[0].Meta.CatalogDetachedAt; got == nil || !got.Equal(at) {
		t.Fatalf("written marker = %v, want the stored %v", got, at)
	}
}

// A detached catalog row is the operator's, so the governance section that
// locks catalog rows no longer applies to it.
func TestApplyGovernsADetachedRowAsUserOwned(t *testing.T) {
	providerID := meta.NewID()
	stored := detachedModel(providerID, time.Now())
	locked := Options{Gov: settingstest.Sections(map[string]any{
		settings.SectionGovernanceModel: &settings.Governance{AllowEdit: false},
	})}

	b := &builder{opts: locked, rows: &Rows{Models: []*model.Model{stored}}, lic: license.Community}
	if _, err := planModel(t, b, stored, modelDoc(providerID, "GPT-4o (renamed)")); err != nil {
		t.Fatalf("edit of a detached row under a locked section = %v, want it allowed", err)
	}

	attached := detachedModel(providerID, time.Now())
	attached.Meta.CatalogDetachedAt = nil
	b = &builder{opts: locked, rows: &Rows{Models: []*model.Model{attached}}, lic: license.Community}
	var gov *GovernanceError
	if _, err := planModel(t, b, attached, modelDoc(providerID, "GPT-4o (renamed)")); !errors.As(err, &gov) {
		t.Fatalf("edit of an attached row under a locked section = %v, want a GovernanceError", err)
	}
}
