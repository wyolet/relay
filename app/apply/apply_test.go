package apply

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/meta"
)

// scopedAuthz allows every verb on rows owned by one team, and nothing else.
type scopedAuthz struct{ teamID string }

func (s scopedAuthz) Authorize(_ context.Context, _ string, res authz.Resource) error {
	if res.Owner != nil && res.Owner.Kind == meta.OwnerTeam && res.Owner.ID == s.teamID {
		return nil
	}
	return authz.ErrForbidden
}

// A row the caller may not read comes back as forbidden — named from their
// own bundle, with nothing about the stored row attached — so a scoped apply
// reports what it ignored instead of looking complete.
func TestAuthorizeReportsUnreadableNonWritesAsForbidden(t *testing.T) {
	mine := meta.Owner{Kind: meta.OwnerTeam, ID: "t-mine"}
	theirs := meta.Owner{Kind: meta.OwnerTeam, ID: "t-theirs"}
	p := &Result{Entries: []Entry{
		{Kind: "Project", Name: "ours", ID: "p-1", Action: ActionUnchanged, plural: "projects", owner: mine},
		{Kind: "Project", Name: "theirs", ID: "p-2", Action: ActionUnchanged, plural: "projects", owner: theirs},
		{Kind: "Project", Name: "theirs-dirty", ID: "p-3", Action: ActionSkipDirty, ChangedFields: []string{"spec.enabled"}, plural: "projects", owner: theirs},
	}}
	if err := Authorize(context.Background(), p, scopedAuthz{teamID: "t-mine"}); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	type reported struct {
		Name, ID string
		Action   Action
		Fields   []string
	}
	var got []reported
	for _, e := range p.Entries {
		got = append(got, reported{e.Name, e.ID, e.Action, e.ChangedFields})
	}
	want := []reported{
		{"ours", "p-1", ActionUnchanged, nil},
		{"theirs", "", ActionForbidden, nil},
		{"theirs-dirty", "", ActionForbidden, nil},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("entries (-want +got):\n%s", diff)
	}
	if want := (Counts{Unchanged: 1, Forbidden: 2}); p.Counts != want {
		t.Fatalf("counts = %+v, want %+v", p.Counts, want)
	}
}

// probe is one Authorize call an authorizer saw.
type probe struct {
	Action, Kind, ID, Name string
	Owner                  meta.Owner
}

// recordingAuthz records every probe and denies the listed actions.
type recordingAuthz struct {
	probes *[]probe
	deny   map[string]bool
}

func (r recordingAuthz) Authorize(_ context.Context, action string, res authz.Resource) error {
	*r.probes = append(*r.probes, probe{action, res.Kind, res.ID, res.Name, *res.Owner})
	if r.deny[action] {
		return authz.ErrForbidden
	}
	return nil
}

// Every write is authorized twice: system.apply at the row's tenancy scope
// (global for anything that is not a team or project), then the row's own
// verb against its owner, or against authorizeAt when the entry carries one.
func TestAuthorizeProbesEachEntry(t *testing.T) {
	system := meta.Owner{Kind: meta.OwnerSystem}
	global := meta.Global
	team := meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}
	proj := meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}
	alice := meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}
	write := func(context.Context) error { return nil }
	entries := func() []Entry {
		return []Entry{
			{Kind: "Key", Name: "k", ID: "k-1", Action: ActionCreate, plural: "keys", owner: proj, write: write},
			{Kind: "HostKey", Name: "hk", ID: "hk-1", Action: ActionUpdate, plural: "host-keys", owner: alice, write: write},
			{Kind: "Project", Name: "p", ID: "p-1", Action: ActionDelete, plural: "projects", owner: team, write: write},
			{Kind: "Account", Name: "share", ID: "a-1", Action: ActionDelete, plural: "accounts", owner: alice, authorizeAt: &team, write: write},
			{Kind: "Policy", Name: "pol", ID: "pol-1", Action: ActionUnchanged, plural: "policies", owner: system},
		}
	}
	wantProbes := []probe{
		{"system.apply", "system", "", "", proj},
		{"keys.create", "key", "k-1", "k", proj},
		{"system.apply", "system", "", "", global},
		{"host-keys.update", "host-key", "hk-1", "hk", alice},
		{"system.apply", "system", "", "", team},
		{"projects.delete", "project", "p-1", "p", team},
		{"system.apply", "system", "", "", team},
		{"accounts.delete", "account", "a-1", "share", team},
		{"policies.get", "policy", "pol-1", "pol", system},
	}

	var probes []probe
	p := &Result{Entries: entries()}
	if err := Authorize(context.Background(), p, recordingAuthz{probes: &probes}); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if diff := cmp.Diff(wantProbes, probes); diff != "" {
		t.Fatalf("probes (-want +got):\n%s", diff)
	}
	if want := (Counts{Create: 1, Update: 1, Delete: 2, Unchanged: 1}); p.Counts != want {
		t.Fatalf("counts = %+v, want %+v", p.Counts, want)
	}
	// The audit row names the row's own owner, not the scope it was authorized at.
	action, kind, owner := p.Entries[3].Authorized()
	if action != "accounts.delete" || kind != "account" || owner != alice {
		t.Fatalf("Authorized() = %s, %s, %+v", action, kind, owner)
	}

	for _, tc := range []struct {
		deny       string
		wantEntry  string
		wantProbes int
	}{
		{"system.apply", "k", 1},
		{"host-keys.update", "hk", 4},
		{"projects.delete", "p", 6},
	} {
		t.Run("denied "+tc.deny, func(t *testing.T) {
			var probes []probe
			p := &Result{Entries: entries()}
			err := Authorize(context.Background(), p, recordingAuthz{probes: &probes, deny: map[string]bool{tc.deny: true}})
			var ae *AuthzError
			if !errors.As(err, &ae) || ae.Entry.Name != tc.wantEntry || !errors.Is(err, authz.ErrForbidden) {
				t.Fatalf("err = %v, want a forbidden AuthzError on %q", err, tc.wantEntry)
			}
			if diff := cmp.Diff(wantProbes[:tc.wantProbes], probes); diff != "" {
				t.Fatalf("probes before the refusal (-want +got):\n%s", diff)
			}
			if len(p.Entries) != 5 || p.Counts != (Counts{}) {
				t.Fatalf("a refused plan was rewritten: %d entries, counts %+v", len(p.Entries), p.Counts)
			}
		})
	}

	// No authorizer is the boot seed, no plan is nothing to authorize.
	if err := Authorize(context.Background(), &Result{Entries: entries()}, nil); err != nil {
		t.Fatalf("nil authorizer: %v", err)
	}
	if err := Authorize(context.Background(), nil, recordingAuthz{probes: &probes}); err != nil {
		t.Fatalf("nil plan: %v", err)
	}
}

func TestVerbOf(t *testing.T) {
	for action, want := range map[Action]Action{
		ActionCreate: ActionCreate, ActionUpdate: ActionUpdate, ActionDelete: ActionDelete,
		ActionUnchanged: ActionUpdate, ActionSkipDirty: ActionUpdate,
	} {
		if got := verbOf(action); got != want {
			t.Errorf("verbOf(%s) = %s, want %s", action, got, want)
		}
	}
}

// teamUpdate is a planned update of the Team name as it looked at seenAt; the write records the name.
func teamUpdate(name string, seenAt time.Time, written *[]string, fail error) Entry {
	return Entry{
		Kind: "Team", Name: name, ID: "id-" + name, Action: ActionUpdate,
		ChangedFields: []string{"metadata.displayName"}, plural: "teams",
		prev: rowState{present: true, updatedAt: seenAt},
		write: func(context.Context) error {
			if fail == nil {
				*written = append(*written, name)
			}
			return fail
		},
	}
}

// Execute re-reads the rows before writing: a row that moved, turned dirty,
// vanished or appeared since the plan is reported as a conflict and left
// alone, while the rest of the run still lands.
func TestExecuteWritesOnlyRowsThatStillMatchThePlan(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	row := func(name string, updated time.Time, dirty bool) *team.Team {
		return &team.Team{Meta: meta.Metadata{ID: "id-" + name, Name: name, UpdatedAt: updated, Dirty: dirty}}
	}
	var written []string
	create := func(name string) Entry {
		return Entry{Kind: "Team", Name: name, Action: ActionCreate, plural: "teams", write: func(context.Context) error {
			written = append(written, name)
			return nil
		}}
	}
	p := &Result{
		Entries: []Entry{
			teamUpdate("same", at, &written, nil),
			teamUpdate("moved", at, &written, nil),
			teamUpdate("dirtied", at, &written, nil),
			teamUpdate("vanished", at, &written, nil),
			create("created"),
			create("appeared"),
			{Kind: "Team", Name: "untouched", Action: ActionUnchanged, plural: "teams"},
		},
		reload: func(context.Context) (*Rows, error) {
			return &Rows{Teams: []*team.Team{
				row("same", at, false), row("moved", at.Add(time.Nanosecond), false),
				row("dirtied", at, true), row("appeared", at, false), row("untouched", at.Add(time.Hour), true),
			}}, nil
		},
	}
	applied, err := Execute(context.Background(), p, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if want := []string{"same", "created"}; !slices.Equal(written, want) {
		t.Fatalf("written = %v, want %v", written, want)
	}
	var names []string
	for _, e := range applied {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, written) {
		t.Fatalf("applied = %v, want %v", names, written)
	}
	wantActions := []Action{ActionUpdate, ActionConflict, ActionConflict, ActionConflict, ActionCreate, ActionConflict, ActionUnchanged}
	for i, e := range p.Entries {
		if e.Action != wantActions[i] {
			t.Errorf("%s: action = %s, want %s", e.Name, e.Action, wantActions[i])
		}
		if e.Action == ActionConflict && (e.ChangedFields != nil || e.write != nil) {
			t.Errorf("%s: a conflict kept its diff or its write", e.Name)
		}
	}
	if want := (Counts{Update: 1, Create: 1, Conflict: 4, Unchanged: 1}); p.Counts != want {
		t.Fatalf("counts = %+v, want %+v", p.Counts, want)
	}
}

func TestExecuteEdges(t *testing.T) {
	if applied, err := Execute(context.Background(), nil, nil); applied != nil || err != nil {
		t.Fatalf("nil plan = %v, %v", applied, err)
	}

	// A hand-built plan has no reload and writes unconditionally.
	var written []string
	p := &Result{Entries: []Entry{teamUpdate("a", time.Time{}, &written, nil)}}
	if applied, err := Execute(context.Background(), p, nil); err != nil || len(applied) != 1 || !slices.Equal(written, []string{"a"}) {
		t.Fatalf("no reload: applied %v, err %v, written %v", applied, err, written)
	}

	boom := errors.New("pool closed")
	written = nil
	p = &Result{
		Entries: []Entry{teamUpdate("a", time.Time{}, &written, nil)},
		reload:  func(context.Context) (*Rows, error) { return nil, boom },
	}
	if applied, err := Execute(context.Background(), p, nil); err != boom || applied != nil || written != nil {
		t.Fatalf("failed re-read: applied %v, err %v, written %v", applied, err, written)
	}

	// A denied write aborts before the re-read and before any write.
	p = &Result{
		Entries: []Entry{teamUpdate("a", time.Time{}, &written, nil)},
		reload: func(context.Context) (*Rows, error) {
			t.Error("re-read after a refusal")
			return &Rows{}, nil
		},
	}
	var ae *AuthzError
	if applied, err := Execute(context.Background(), p, scopedAuthz{teamID: "t-mine"}); !errors.As(err, &ae) || applied != nil || written != nil {
		t.Fatalf("denied write: applied %v, err %v, written %v", applied, err, written)
	}
}

// A store that fails part-way through still reports what already landed —
// the caller re-applies and converges.
func TestExecuteReportsWhatLandedBeforeAStoreFailure(t *testing.T) {
	boom := errors.New("pool closed")
	var written []string
	p := &Result{Entries: []Entry{
		teamUpdate("first", time.Time{}, &written, nil),
		teamUpdate("second", time.Time{}, &written, boom),
		teamUpdate("third", time.Time{}, &written, nil),
	}}
	applied, err := Execute(context.Background(), p, nil)
	var se *StoreError
	if !errors.As(err, &se) || !errors.Is(err, boom) {
		t.Fatalf("err = %v, want a StoreError wrapping the store's", err)
	}
	if se.Entry.Name != "second" || len(se.Applied) != 1 || se.Applied[0].Name != "first" {
		t.Fatalf("StoreError = failed %q after %+v, want second after first", se.Entry.Name, se.Applied)
	}
	if len(applied) != 1 || applied[0].Name != "first" || !slices.Equal(written, []string{"first"}) {
		t.Fatalf("applied = %+v, written = %v, want just the first", applied, written)
	}
	if got, want := err.Error(), `apply: write Team "second": pool closed`; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}
}

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

const checkDocsProvider = `
apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata:
  name: acme
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Model
metadata:
  name: acme-1
  owner: {kind: provider, name: acme}
spec:
  snapshots: [{name: acme-1}]
  pointer: `

func TestCheckDocsAcceptsASeedableTree(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(checkDocsProvider + "acme-1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDocs(context.Background(), docs); err != nil {
		t.Fatalf("CheckDocs = %v, want nil", err)
	}
}

// A row the graph linter passes but its own Validate refuses fails the seed, so it must fail the check.
func TestCheckDocsRefusesWhatValidateRefuses(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(checkDocsProvider + "acme-2\n"))
	if err != nil {
		t.Fatal(err)
	}
	var invalid *InvalidError
	if err := CheckDocs(context.Background(), docs); !errors.As(err, &invalid) || invalid.Name != "acme-1" {
		t.Fatalf("CheckDocs = %v, want InvalidError for acme-1", err)
	}
}
