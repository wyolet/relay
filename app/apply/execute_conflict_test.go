package apply

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/team"
)

// planWithReload builds a one-entry update plan for a Team and points its
// re-read at rows the test controls, so Execute's conflict check can be
// exercised without Postgres.
func planWithReload(t *testing.T, planned, atWrite *team.Team) (*Result, *[]*team.Team) {
	t.Helper()
	yes := true
	incoming := &team.Team{
		Meta: meta.Metadata{ID: planned.Meta.ID, Name: planned.Meta.Name, DisplayName: "Renamed"},
		Spec: team.Spec{Enabled: &yes},
	}
	var written []*team.Team
	e := Entry{
		Kind: "Team", Name: planned.Meta.Name, ID: planned.Meta.ID, Action: ActionUpdate,
		ChangedFields: []string{"metadata.displayName"},
		plural:        "teams",
		prev:          rowState{present: true, dirty: planned.Meta.Dirty, updatedAt: planned.Meta.UpdatedAt},
		write: func(context.Context) error {
			written = append(written, incoming)
			return nil
		},
	}
	p := &Result{
		Entries: []Entry{e},
		reload: func(context.Context) (*Rows, error) {
			return &Rows{Teams: []*team.Team{atWrite}}, nil
		},
	}
	recount(p)
	return p, &written
}

func TestExecuteWritesWhenTheRowIsUnchanged(t *testing.T) {
	at := time.Now().UTC()
	row := &team.Team{Meta: meta.Metadata{ID: "t-1", Name: "platform", UpdatedAt: at}}
	p, written := planWithReload(t, row, row)

	applied, err := Execute(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(applied) != 1 || len(*written) != 1 {
		t.Fatalf("applied=%d written=%d, want 1 and 1", len(applied), len(*written))
	}
	if p.Counts.Conflict != 0 {
		t.Fatalf("conflicts = %d, want 0", p.Counts.Conflict)
	}
}

func TestExecuteReportsConflictWhenTheRowMovedSincePlan(t *testing.T) {
	at := time.Now().UTC()
	planned := &team.Team{Meta: meta.Metadata{ID: "t-1", Name: "platform", UpdatedAt: at}}
	// Another writer touched the row between Plan and Execute.
	moved := &team.Team{Meta: meta.Metadata{ID: "t-1", Name: "platform", UpdatedAt: at.Add(time.Second), Dirty: true}}
	p, written := planWithReload(t, planned, moved)

	applied, err := Execute(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(applied) != 0 || len(*written) != 0 {
		t.Fatalf("applied=%d written=%d, want nothing written", len(applied), len(*written))
	}
	if p.Entries[0].Action != ActionConflict {
		t.Fatalf("action = %q, want %q", p.Entries[0].Action, ActionConflict)
	}
	if p.Counts.Conflict != 1 || p.Counts.Update != 0 {
		t.Fatalf("counts = %+v, want one conflict and no update", p.Counts)
	}
}

func TestExecuteReportsConflictWhenTheRowVanishedSincePlan(t *testing.T) {
	planned := &team.Team{Meta: meta.Metadata{ID: "t-1", Name: "platform", UpdatedAt: time.Now().UTC()}}
	gone := &team.Team{Meta: meta.Metadata{ID: "t-2", Name: "other"}}
	p, written := planWithReload(t, planned, gone)

	if _, err := Execute(context.Background(), p, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(*written) != 0 {
		t.Fatalf("wrote %d rows over a deleted row, want 0", len(*written))
	}
	if p.Entries[0].Action != ActionConflict {
		t.Fatalf("action = %q, want %q", p.Entries[0].Action, ActionConflict)
	}
}

// Two plans built against the same row and executed concurrently must not
// both write: the second one's re-read sees the first one's change.
func TestConcurrentExecutesWriteAtMostOnce(t *testing.T) {
	at := time.Now().UTC()
	var mu sync.Mutex
	stored := &team.Team{Meta: meta.Metadata{ID: "t-1", Name: "platform", UpdatedAt: at}}

	newPlan := func() *Result {
		e := Entry{
			Kind: "Team", Name: "platform", ID: "t-1", Action: ActionUpdate, plural: "teams",
			prev: rowState{present: true, updatedAt: at},
			write: func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				stored = &team.Team{Meta: meta.Metadata{
					ID: "t-1", Name: "platform", UpdatedAt: stored.Meta.UpdatedAt.Add(time.Second),
				}}
				return nil
			},
		}
		p := &Result{Entries: []Entry{e}, reload: func(context.Context) (*Rows, error) {
			mu.Lock()
			defer mu.Unlock()
			return &Rows{Teams: []*team.Team{stored}}, nil
		}}
		recount(p)
		return p
	}

	// Serialized the way the handler's single-flight mutex serializes them:
	// the second plan is stale and must report a conflict, not overwrite.
	first, second := newPlan(), newPlan()
	if _, err := Execute(context.Background(), first, nil); err != nil {
		t.Fatalf("first Execute: %v", err)
	}
	if _, err := Execute(context.Background(), second, nil); err != nil {
		t.Fatalf("second Execute: %v", err)
	}
	if first.Counts.Conflict != 0 {
		t.Fatalf("first run conflicted: %+v", first.Counts)
	}
	if second.Counts.Conflict != 1 {
		t.Fatalf("second run counts = %+v, want one conflict", second.Counts)
	}
}

// A store that fails part-way through still reports what already landed —
// the caller re-applies and converges.
func TestExecuteReportsWhatLandedBeforeAStoreFailure(t *testing.T) {
	at := time.Now().UTC()
	rows := &Rows{Teams: []*team.Team{
		{Meta: meta.Metadata{ID: "t-1", Name: "first", UpdatedAt: at}},
		{Meta: meta.Metadata{ID: "t-2", Name: "second", UpdatedAt: at}},
	}}
	entry := func(name string, fail bool) Entry {
		return Entry{
			Kind: "Team", Name: name, Action: ActionUpdate, plural: "teams",
			prev: rowState{present: true, updatedAt: at},
			write: func(context.Context) error {
				if fail {
					return errors.New("pool closed")
				}
				return nil
			},
		}
	}
	p := &Result{
		Entries: []Entry{entry("first", false), entry("second", true)},
		reload:  func(context.Context) (*Rows, error) { return rows, nil },
	}
	recount(p)

	applied, err := Execute(context.Background(), p, nil)
	var se *StoreError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a StoreError", err)
	}
	if se.Entry.Name != "second" {
		t.Fatalf("failed entry = %q, want second", se.Entry.Name)
	}
	if len(applied) != 1 || applied[0].Name != "first" {
		t.Fatalf("applied = %+v, want just the first", applied)
	}
}
