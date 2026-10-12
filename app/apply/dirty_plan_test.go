package apply

import (
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
)

// dirtyRateLimit is the stored row rateLimitDoc("burst") declares, marked as edited through the control API.
func dirtyRateLimit() *ratelimit.RateLimit {
	return &ratelimit.RateLimit{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "burst", DisplayName: "burst", Dirty: true,
			Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"},
		},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: "requests", Amount: 10, Window: ratelimit.Window(time.Minute),
			Strategy: "token-bucket",
		}}},
	}
}

// An operator edit that was exported and applied back matches the document, so it is not drift.
func TestApplyPlansADirtyRowEqualToTheManifestAsUnchanged(t *testing.T) {
	b := &builder{rows: &Rows{RateLimits: []*ratelimit.RateLimit{dirtyRateLimit()}}, lic: license.Community}
	entries, written, err := rateLimitPlan(t, b, rateLimitDoc("burst"))
	if err != nil {
		t.Fatalf("planKind: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != ActionUnchanged || len(entries[0].ChangedFields) != 0 {
		t.Fatalf("entries = %+v, want one unchanged row", entries)
	}
	if len(written) != 0 {
		t.Fatalf("an unchanged dirty row was written: %+v", written[0])
	}
}

func TestApplySkipsADirtyRowThatDiffers(t *testing.T) {
	doc := rateLimitDoc("burst")
	doc.Metadata.DisplayName = "Burst (renamed)"
	b := &builder{rows: &Rows{RateLimits: []*ratelimit.RateLimit{dirtyRateLimit()}}, lic: license.Community}
	entries, written, err := rateLimitPlan(t, b, doc)
	if err != nil {
		t.Fatalf("planKind: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != ActionSkipDirty {
		t.Fatalf("entries = %+v, want one skip-dirty row", entries)
	}
	if !slices.Equal(entries[0].ChangedFields, []string{"metadata.displayName"}) {
		t.Fatalf("changedFields = %v, want the differing path", entries[0].ChangedFields)
	}
	if len(written) != 0 {
		t.Fatalf("a dirty row was overwritten without force: %+v", written[0])
	}
}

func TestApplyForceWritesADirtyRowThatDiffers(t *testing.T) {
	doc := rateLimitDoc("burst")
	doc.Metadata.DisplayName = "Burst (renamed)"
	b := &builder{opts: Options{Force: true}, rows: &Rows{RateLimits: []*ratelimit.RateLimit{dirtyRateLimit()}}, lic: license.Community}
	entries, written, err := rateLimitPlan(t, b, doc)
	if err != nil {
		t.Fatalf("planKind: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != ActionUpdate {
		t.Fatalf("entries = %+v, want one update", entries)
	}
	if len(written) != 1 || written[0].Meta.DisplayName != "Burst (renamed)" || written[0].Meta.Dirty {
		t.Fatalf("forced write = %+v, want the document's row with the dirty flag cleared", written)
	}
}
