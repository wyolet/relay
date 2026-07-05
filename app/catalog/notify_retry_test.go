package catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
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

// TestNotify_TransientApplyFailureEventuallyApplied reproduces the audit
// finding "NOTIFY events dropped permanently on transient applyEvent
// failure" (audit 2026-07-04, audit-catalog.md, notify.go applyDrained).
//
// A NOTIFY event is drained from the debouncer and applyEvent fails once
// with a transient store error. The listener's whole job is convergence, so
// the event must eventually be applied (re-queued into the debouncer, or a
// full Reload scheduled). Today applyDrained only logs the error and the
// event vanishes: the snapshot diverges from PG until an unrelated event
// for the same row or a manual /reload.
func TestNotify_TransientApplyFailureEventuallyApplied(t *testing.T) {
	t.Skip("audit 2026-07-04: NOTIFY events dropped on transient applyEvent failure — known-broken, unskip with the fix")
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

	// First flush cycle: store.Get fails transiently; the row is available
	// on every subsequent call.
	l.applyDrained(ctx)

	// Subsequent flush cycles: a converging listener re-applies the failed
	// event (or falls back to a reload). Drive the flush deterministically —
	// no listener goroutine, no PG.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.applyDrained(ctx)
		if _, ok := c.Current().Model(m3.Meta.ID); ok {
			return // converged
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("model %s never reached the snapshot after a transient applyEvent failure: "+
		"the drained NOTIFY event was dropped permanently (store.Get calls: %d — never retried)",
		m3.Meta.ID, getter.calls)
}
