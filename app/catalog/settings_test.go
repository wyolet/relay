package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/settings"
)

// fakeSettingsStore is a minimal SettingsLister: List returns whatever
// rows are set; Get returns the row for the requested section.
type fakeSettingsStore struct {
	rows map[string]*settings.Row
}

func (f *fakeSettingsStore) List(context.Context) ([]*settings.Row, error) {
	out := make([]*settings.Row, 0, len(f.rows))
	for _, r := range f.rows {
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeSettingsStore) Get(_ context.Context, section string) (*settings.Row, error) {
	if r, ok := f.rows[section]; ok {
		return r, nil
	}
	sec, _ := settings.Lookup(section)
	return &settings.Row{Section: section, Value: sec.Defaults()}, nil
}

func newHolder(rows map[string]*settings.Row) *settingsHolder {
	if rows == nil {
		rows = map[string]*settings.Row{}
	}
	return &settingsHolder{store: &fakeSettingsStore{rows: rows}}
}

func TestSettingsHolder_NotifiesOnUpsertAndDelete(t *testing.T) {
	h := newHolder(nil)
	var fired int
	h.subscribe(settings.SectionParsing, func() { fired++ })

	// Upsert: callback runs, and the new value is visible inside it.
	h.rows()[settings.SectionParsing] = &settings.Row{Section: settings.SectionParsing, Value: &settings.Parsing{RichParsing: false}}
	if err := h.applyUpsert(context.Background(), settings.SectionParsing); err != nil {
		t.Fatalf("applyUpsert: %v", err)
	}
	if fired != 1 {
		t.Fatalf("upsert fired %d, want 1", fired)
	}
	if v, _ := h.loadSection(settings.SectionParsing); v.(*settings.Parsing).RichParsing {
		t.Fatal("callback should see the upserted value (RichParsing=false)")
	}

	// Delete: callback runs again.
	h.applyDelete(settings.SectionParsing)
	if fired != 2 {
		t.Fatalf("delete fired %d total, want 2", fired)
	}

	// A change to an unsubscribed section does not fire this callback.
	h.applyDelete(settings.SectionPayloadLogging)
	if fired != 2 {
		t.Fatalf("unrelated section fired callback: %d", fired)
	}
}

func TestSettingsHolder_ReloadNotifiesSubscribers(t *testing.T) {
	rows := map[string]*settings.Row{
		settings.SectionParsing: {Section: settings.SectionParsing, Value: &settings.Parsing{RichParsing: false}},
	}
	h := newHolder(rows)

	var fired int
	h.subscribe(settings.SectionParsing, func() { fired++ })

	// reload is the boot path: subscribers registered beforehand get the
	// stored value via notifyAll.
	if err := h.reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if fired != 1 {
		t.Fatalf("reload fired %d, want 1", fired)
	}
	if v, _ := h.loadSection(settings.SectionParsing); v.(*settings.Parsing).RichParsing {
		t.Fatal("reload should have loaded stored RichParsing=false")
	}
}

// rows exposes the fake store's row map for in-test mutation.
func (h *settingsHolder) rows() map[string]*settings.Row {
	return h.store.(*fakeSettingsStore).rows
}
