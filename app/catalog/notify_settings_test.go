package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/settings"
)

// A bulk drain falls back to one full Reload, which rebuilds catalog rows
// only — the settings cache is loaded separately.
// TestApplyDrainedAppliesSettingsInABulkBurst pins that a section change
// riding in such a burst still reaches the cache.
func TestApplyDrainedAppliesSettingsInABulkBurst(t *testing.T) {
	c := bughuntCatalog(t, nil, nil, nil, nil, nil)
	rows := map[string]*settings.Row{
		settings.SectionParsing: {Section: settings.SectionParsing, Value: &settings.Parsing{RichParsing: false}},
	}
	c.settings.store = &fakeSettingsStore{rows: rows}
	l := &Listener{cat: c, deb: newDebouncer(0)}

	for i := 0; i < reloadBatchThreshold; i++ {
		l.deb.push(notifyEvent{Kind: "team", Op: "delete", ID: meta.NewID()})
	}
	l.deb.push(notifyEvent{Kind: "settings", Op: "upsert", ID: settings.SectionParsing})
	l.applyDrained(context.Background())

	v, ok := c.Setting(settings.SectionParsing)
	if !ok {
		t.Fatal("settings event was dropped by the bulk reload fallback")
	}
	// The default is on; the upserted row turns it off.
	p, _ := v.(*settings.Parsing)
	if p == nil || p.RichParsing {
		t.Fatalf("section = %+v, want the upserted value (richParsing off)", v)
	}
}
