package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/team"
)

// Every kind parseEvent accepts must reach a handler. A kind added to
// validKinds without a case in applyEvent would otherwise make its NOTIFY a
// silent no-op — the failure mode is invisible in production.
func TestApplyEventHandlesEveryValidKind(t *testing.T) {
	c := tenancyCatalog(t, nil, nil, nil, nil, nil)
	l := &Listener{cat: c, deb: newDebouncer(0)}

	for kind := range validKinds {
		// Delete needs no store read, so this exercises dispatch alone.
		id := "some-id"
		if kind == "overlay" {
			id = "model|some-id" // composite payload
		}
		if err := l.applyEvent(context.Background(), drainedEvent{Kind: kind, Op: "delete", ID: id}); err != nil {
			t.Errorf("applyEvent(%s delete): %v", kind, err)
		}
	}
}

// Past the batch threshold a drained burst is one full rebuild, not one
// snapshot clone per row.
func TestApplyDrainedReloadsPastTheBatchThreshold(t *testing.T) {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	c := tenancyCatalog(t, []*team.Team{tm}, nil, nil, nil, nil)
	l := &Listener{cat: c, deb: newDebouncer(0)}

	for i := 0; i <= reloadBatchThreshold; i++ {
		l.deb.push(notifyEvent{Kind: "team", Op: "delete", ID: meta.NewID()})
	}
	l.applyDrained(context.Background())

	// A rebuild reads the stores again, so the team the incremental deletes
	// would have removed is back.
	if _, ok := c.Current().Team(tm.Meta.ID); !ok {
		t.Fatal("a bulk drain did not fall back to a full reload")
	}
}

func TestApplyDrainedStaysIncrementalBelowTheThreshold(t *testing.T) {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	c := tenancyCatalog(t, []*team.Team{tm}, nil, nil, nil, nil)
	l := &Listener{cat: c, deb: newDebouncer(0)}

	l.deb.push(notifyEvent{Kind: "team", Op: "delete", ID: tm.Meta.ID})
	l.applyDrained(context.Background())

	if _, ok := c.Current().Team(tm.Meta.ID); ok {
		t.Fatal("a small drain should have applied the delete incrementally")
	}
}
