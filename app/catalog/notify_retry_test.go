package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/settings"
)

// mutModList is a ModelLister whose contents can change after the initial
// Reload — it stands in for PG state that a concurrent writer mutates.
type mutModList struct{ models []*model.Model }

func (l *mutModList) List(context.Context) ([]*model.Model, error) { return l.models, nil }

// flakyModelGetter fails the first Get with a transient error (PG failover,
// timeout), then serves the row — the store equivalent of "retry would have
// succeeded".
type flakyModelGetter struct {
	m     *model.Model
	calls int
}

func (f *flakyModelGetter) Get(_ context.Context, id string) (*model.Model, error) {
	f.calls++
	if f.calls == 1 {
		return nil, errors.New("transient pg error: connection reset by peer")
	}
	if f.m != nil && f.m.Meta.ID == id {
		return f.m, nil
	}
	return nil, nil
}

// A drained NOTIFY event whose apply fails with a transient store error is
// gone from the debouncer, so the listener must still converge on PG state.
func TestNotify_TransientApplyFailureEventuallyApplied(t *testing.T) {
	ctx := context.Background()
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()

	mut := &mutModList{models: models}
	c := New(provs, hosts, pols, mut, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	// A concurrent admin write commits a new model; PG NOTIFY fires.
	m3 := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-late",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provs[0].Meta.ID},
		},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "gpt-late-2025-01-01", OriginalName: "gpt-late-2025-01-01"}},
			Pointer:   "gpt-late-2025-01-01",
		},
	}
	mut.models = append(append([]*model.Model{}, models...), m3)
	getter := &flakyModelGetter{m: m3}

	l := &Listener{
		cat:    c,
		deb:    newDebouncer(time.Second),
		stores: listenerStores{model: getter},
	}
	l.deb.push(notifyEvent{Kind: "model", Op: "upsert", ID: m3.Meta.ID})

	// store.Get fails on this flush; the fallback must still land the row.
	l.applyDrained(ctx)
	if _, ok := c.Current().Model(m3.Meta.ID); !ok {
		t.Fatalf("model %s missing after a transient apply failure (store.Get calls: %d)", m3.Meta.ID, getter.calls)
	}
}

// failingModelList fails List while fail is set — a store outage that also
// breaks the fallback reload.
type failingModelList struct {
	mutModList
	fail bool
}

func (l *failingModelList) List(ctx context.Context) ([]*model.Model, error) {
	if l.fail {
		return nil, errors.New("pg unavailable")
	}
	return l.mutModList.List(ctx)
}

// When the fallback reload fails too, the next flush retries it even with no
// new events, so the pod converges once the store recovers.
func TestNotify_FailedFallbackReloadRetriedNextFlush(t *testing.T) {
	ctx := context.Background()
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()

	list := &failingModelList{mutModList: mutModList{models: models}}
	c := New(provs, hosts, pols, list, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("initial reload: %v", err)
	}

	m3 := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-late",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provs[0].Meta.ID},
		},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "gpt-late-2025-01-01", OriginalName: "gpt-late-2025-01-01"}},
			Pointer:   "gpt-late-2025-01-01",
		},
	}
	list.models = append(append([]*model.Model{}, models...), m3)
	list.fail = true

	l := &Listener{
		cat:    c,
		deb:    newDebouncer(time.Second),
		stores: listenerStores{model: &flakyModelGetter{m: m3}},
	}
	l.deb.push(notifyEvent{Kind: "model", Op: "upsert", ID: m3.Meta.ID})
	l.applyDrained(ctx)
	if _, ok := c.Current().Model(m3.Meta.ID); ok {
		t.Fatal("precondition: model applied although every store read failed")
	}

	list.fail = false
	l.applyDrained(ctx) // no new events
	if _, ok := c.Current().Model(m3.Meta.ID); !ok {
		t.Fatalf("model %s missing after the store recovered", m3.Meta.ID)
	}
}

// flakySettingsStore fails the first failGets Gets, then serves the fake
// store's rows.
type flakySettingsStore struct {
	fakeSettingsStore
	failGets int
	gets     int
}

func (f *flakySettingsStore) Get(ctx context.Context, section string) (*settings.Row, error) {
	f.gets++
	if f.gets <= f.failGets {
		return nil, errors.New("transient pg error: connection reset by peer")
	}
	return f.fakeSettingsStore.Get(ctx, section)
}

// A full reload does not refresh the settings cache, so a settings event
// whose apply fails must go back into the debouncer and land on a later flush.
func TestNotify_FailedSettingsEventRequeued(t *testing.T) {
	ctx := context.Background()
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(ctx); err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	// Fails the incremental apply and the retry after the fallback reload.
	store := &flakySettingsStore{failGets: 2, fakeSettingsStore: fakeSettingsStore{rows: map[string]*settings.Row{
		settings.SectionParsing: {Section: settings.SectionParsing, Value: &settings.Parsing{RichParsing: false}},
	}}}
	c.settings.store = store

	l := &Listener{cat: c, deb: newDebouncer(time.Second)}
	l.deb.push(notifyEvent{Kind: "settings", Op: "upsert", ID: settings.SectionParsing})
	l.applyDrained(ctx)
	l.applyDrained(ctx)
	if _, ok := c.settings.load()[settings.SectionParsing]; !ok {
		t.Fatalf("settings section %q never applied after a transient failure (store.Get calls: %d)", settings.SectionParsing, store.gets)
	}
}
