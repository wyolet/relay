// execute_concurrent_test.go runs Execute from several goroutines against one
// shared row store, with a control-plane edit landing in the Plan→Execute
// window. The guard has two halves — the handler's single-flight mutex
// and Execute's per-write re-read — so the tests here mirror the handler's
// serialization and then check what the re-read alone must catch. Only
// `-race` observes a Result or a store being shared unsafely; the
// serialized single-writer cases live in execute_conflict_test.go.
package apply

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/team"
)

// rowStore is the shared row every plan targets: one Team, guarded, with an
// updated_at clock so a write is visible to the next re-read.
type rowStore struct {
	mu     sync.Mutex
	row    team.Team
	writes int
	// undirtied counts writes that landed over an operator edit. A losing
	// run must never do this: it silently reverts the operator's edit.
	undirtied int
}

func newRowStore(at time.Time) *rowStore {
	return &rowStore{row: team.Team{Meta: meta.Metadata{ID: "t-1", Name: "platform", UpdatedAt: at}}}
}

func (s *rowStore) snapshot() team.Team {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.row
}

func (s *rowStore) write(displayName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.row.Meta.Dirty {
		s.undirtied++
	}
	s.row = team.Team{Meta: meta.Metadata{
		ID: "t-1", Name: "platform", DisplayName: displayName,
		UpdatedAt: s.row.Meta.UpdatedAt.Add(time.Millisecond),
	}}
	s.writes++
}

// externalEdit is the concurrent control-plane PUT: it bumps updated_at and
// marks the row dirty, which is what Execute's re-read must notice.
func (s *rowStore) externalEdit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.row.Meta.UpdatedAt = s.row.Meta.UpdatedAt.Add(time.Millisecond)
	s.row.Meta.Dirty = true
	s.row.Meta.DisplayName = "edited by hand"
}

// plan builds a one-entry update against whatever the store holds now —
// the Plan half of the window.
func (s *rowStore) plan(displayName string) *Result {
	seen := s.snapshot()
	e := Entry{
		Kind: "Team", Name: seen.Meta.Name, ID: seen.Meta.ID, Action: ActionUpdate,
		ChangedFields: []string{"metadata.displayName"},
		plural:        "teams",
		prev:          rowState{present: true, dirty: seen.Meta.Dirty, updatedAt: seen.Meta.UpdatedAt},
		write: func(context.Context) error {
			s.write(displayName)
			return nil
		},
	}
	p := &Result{Entries: []Entry{e}, reload: func(context.Context) (*Rows, error) {
		row := s.snapshot()
		return &Rows{Teams: []*team.Team{&row}}, nil
	}}
	recount(p)
	return p
}

// TestConcurrentAppliesWriteTheRowOnce plans N runs against one row
// state and executes them the way the handler does — concurrent callers,
// serialized by the single-flight mutex. Exactly one write may land; every
// other run's re-read sees the winner's version and reports a conflict.
func TestConcurrentAppliesWriteTheRowOnce(t *testing.T) {
	const runs = 16
	store := newRowStore(time.Now().UTC())

	plans := make([]*Result, runs)
	for i := range plans {
		plans[i] = store.plan("run")
	}

	// Mirrors app/httpapi/control.applyMu: Plan and Execute of two bundles
	// may not interleave within a process.
	var singleFlight sync.Mutex
	var wg sync.WaitGroup
	for i := range plans {
		wg.Add(1)
		go func(p *Result) {
			defer wg.Done()
			singleFlight.Lock()
			defer singleFlight.Unlock()
			if _, err := Execute(context.Background(), p, nil); err != nil {
				t.Errorf("Execute: %v", err)
			}
		}(plans[i])
	}
	wg.Wait()

	store.mu.Lock()
	writes, undirtied := store.writes, store.undirtied
	store.mu.Unlock()

	if writes != 1 {
		t.Fatalf("the row was written %d times from one planned state, want 1", writes)
	}
	if undirtied != 0 {
		t.Fatalf("%d runs wrote over an operator edit", undirtied)
	}
	conflicts := 0
	for _, p := range plans {
		conflicts += p.Counts.Conflict
	}
	if conflicts != runs-1 {
		t.Fatalf("conflicts = %d, want %d (every loser reports one)", conflicts, runs-1)
	}
}

// TestExternalEditInThePlanExecuteWindow is the half the single-flight
// mutex cannot cover: a CRUD edit from another pod (or another handler)
// between Plan and Execute. Nothing may be written and the dirty flag may
// not be cleared — the older diff would otherwise revert the operator.
func TestExternalEditInThePlanExecuteWindow(t *testing.T) {
	const runs = 8
	store := newRowStore(time.Now().UTC())
	plans := make([]*Result, runs)
	for i := range plans {
		plans[i] = store.plan("run")
	}
	store.externalEdit()

	var wg sync.WaitGroup
	for i := range plans {
		wg.Add(1)
		go func(p *Result) {
			defer wg.Done()
			if _, err := Execute(context.Background(), p, nil); err != nil {
				t.Errorf("Execute: %v", err)
			}
		}(plans[i])
	}
	wg.Wait()

	store.mu.Lock()
	writes, dirty := store.writes, store.row.Meta.Dirty
	store.mu.Unlock()
	if writes != 0 {
		t.Fatalf("%d writes landed over a concurrent operator edit, want 0", writes)
	}
	if !dirty {
		t.Fatal("the operator's dirty flag was cleared")
	}
	for i, p := range plans {
		if p.Counts.Conflict != 1 {
			t.Fatalf("plan %d counts = %+v, want one conflict", i, p.Counts)
		}
	}
}
