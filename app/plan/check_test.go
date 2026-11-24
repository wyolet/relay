package plan

import (
	"encoding/json"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

var ignoreMessage = cmpopts.IgnoreFields(engine.Issue{}, "Message")

func newKeyIn(b *basic, accountID string) *key.Key {
	k := &key.Key{Meta: md("key-"+seq(), system), Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: b.sa.Meta.ID}, AccountID: accountID}}
	k.Spec.KeyHash = keyHash(k.Meta.ID)
	return k
}

// An error the proposed rows raise is new; a warning they raise is not reported.
func TestCheck_NewErrorsOnly(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	gone := meta.NewID()
	k := newKeyIn(b, gone)
	elsewhere := &pool.Pool{Meta: md("pool-"+seq(), system), Spec: pool.Spec{HostID: b.hostA.Meta.ID, AccountIDs: []string{b.acctB.Meta.ID}}}

	got, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), nil, []any{k, elsewhere})
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.Issue{{Severity: engine.SeverityError, Code: IssueRefMissing, Source: engine.Ref{Kind: KindKey, ID: k.Meta.ID}, Target: engine.Ref{Kind: KindAccount, ID: gone}, Field: "spec.accountId"}}
	if diff := cmp.Diff(want, got, ignoreMessage); diff != "" {
		t.Errorf("new issues (-want +got):\n%s", diff)
	}
}

// An issue the live view already has is not new, whether or not the proposed rows touch its row.
func TestCheck_ExistingIssueIsNotNew(t *testing.T) {
	b := newBasic()
	broken := newKeyIn(b, meta.NewID())
	b.w.keys = append(b.w.keys, broken)
	h := b.w.start(t)
	if len(h.e.View().AllIssues()) == 0 {
		t.Fatal("the live view has no issue to keep")
	}
	same := *broken
	fine := newKeyIn(b, b.w.saAccount(b.sa).Meta.ID)

	got, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), nil, []any{&same, fine})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]engine.Issue(nil), got); diff != "" {
		t.Errorf("new issues (-want +got):\n%s", diff)
	}
}

// A proposed row that breaks a row already serving is refused through the issue it raises on that row.
func TestCheck_BreaksAnExistingRow(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	lane := b.w.saAccount(b.sa)
	twin := &account.Account{Meta: md(lane.Meta.Name, system), Spec: account.Spec{HostID: b.hostA.Meta.ID}}

	got, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), nil, []any{twin})
	if err != nil {
		t.Fatal(err)
	}
	onLane := engine.Issue{Severity: engine.SeverityError, Code: "duplicate_name", Source: engine.Ref{Kind: KindAccount, ID: lane.Meta.ID}, Field: "metadata.name"}
	var found []engine.Issue
	for _, i := range got {
		if i.Source == onLane.Source {
			found = append(found, i)
		}
	}
	if diff := cmp.Diff([]engine.Issue{onLane}, found, ignoreMessage, cmpopts.IgnoreFields(engine.Issue{}, "Target")); diff != "" {
		t.Errorf("issues on the existing lane (-want +got):\n%s", diff)
	}
}

// A check leaves the live engine as it was and no goroutine behind.
func TestCheck_LeavesLiveAlone(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	before := h.e.View()
	beforeIssues := before.AllIssues()
	goroutines := runtime.NumGoroutine()
	cells := h.cells.Len()

	c := NewCheck(false, time.Minute)
	for range 3 {
		if _, err := c.NewIssues(t.Context(), h.e.View(), []any{b.hostA}, []any{newKeyIn(b, meta.NewID())}); err != nil {
			t.Fatal(err)
		}
	}
	after := h.e.View()
	if after != before || after.Gen() != before.Gen() {
		t.Errorf("live generation moved from %d to %d", before.Gen(), after.Gen())
	}
	if diff := cmp.Diff(beforeIssues, after.AllIssues()); diff != "" {
		t.Errorf("live issues changed (-before +after):\n%s", diff)
	}
	if n := h.cells.Len(); n != cells {
		t.Errorf("live cells went from %d to %d", cells, n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > goroutines && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > goroutines {
		t.Errorf("%d goroutines after the checks, %d before", n, goroutines)
	}
}

// A check waits for the one before it; past its bound it answers ErrCheckTimeout, and the next one runs once the slot frees.
func TestCheck_TimeoutAndOneAtATime(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	c := NewCheck(false, 20*time.Millisecond)
	c.slot <- struct{}{}
	if _, err := c.NewIssues(t.Context(), h.e.View(), nil, nil); !errors.Is(err, ErrCheckTimeout) {
		t.Fatalf("while another check runs: %v, want ErrCheckTimeout", err)
	}
	<-c.slot
	c.timeout = time.Minute
	if _, err := c.NewIssues(t.Context(), h.e.View(), nil, nil); err != nil {
		t.Fatalf("once the slot is free: %v", err)
	}
}

// A build that outlives the bound answers ErrCheckTimeout and keeps the slot until it ends; checks asked for meanwhile answer at once and start no build; once it ends, the next check builds.
func TestCheck_BuildOutlivesTheBound(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	const bound = 200 * time.Millisecond
	c := NewCheck(false, bound)
	gate := make(chan struct{})
	var builds atomic.Int32
	c.build = func(src Sources) (*engine.View, error) {
		builds.Add(1)
		<-gate
		return scratchView(src)
	}
	if _, err := c.NewIssues(t.Context(), h.e.View(), nil, nil); !errors.Is(err, ErrCheckTimeout) {
		t.Fatalf("a build past the bound: %v, want ErrCheckTimeout", err)
	}
	start := time.Now()
	if _, err := c.NewIssues(t.Context(), h.e.View(), nil, nil); !errors.Is(err, ErrCheckTimeout) {
		t.Fatalf("while it runs: %v, want ErrCheckTimeout", err)
	}
	if _, _, err := c.Hold(t.Context()); !errors.Is(err, ErrCheckTimeout) {
		t.Fatalf("Hold while it runs: %v, want ErrCheckTimeout", err)
	}
	if waited := time.Since(start); waited >= bound/2 {
		t.Errorf("checks asked for while it runs waited %v, want an answer at once", waited)
	}
	if n := builds.Load(); n != 1 {
		t.Fatalf("%d builds started, want 1", n)
	}
	close(gate)
	deadline := time.Now().Add(5 * time.Second)
	for c.overrun.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := c.NewIssues(t.Context(), h.e.View(), nil, nil); err != nil {
		t.Fatalf("once it ended: %v", err)
	}
	if n := builds.Load(); n != 2 {
		t.Errorf("%d builds started, want 2", n)
	}
}

// A lane bound to a disabled policy serves nothing though no row files an error: it refuses, on the binding. A lane that reaches no key refuses nothing.
func TestCheck_RefusedLane(t *testing.T) {
	b := newBasic()
	off := false
	disabled := b.w.policy("off-"+seq(), policy.Spec{Enabled: &off})
	h := b.w.start(t)
	scope := meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID}
	sa := &serviceaccount.ServiceAccount{Meta: md("sa-"+seq(), scope), Spec: serviceaccount.Spec{ProjectID: b.proj.Meta.ID}}
	lane := account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, "", b.proj.Meta.ID)
	lane.Meta.ID = meta.NewID()
	pb := &policybinding.PolicyBinding{Meta: md("pb-"+seq(), scope), Spec: policybinding.Spec{PolicyID: disabled.Meta.ID, AccountIDs: []string{lane.Meta.ID}}}
	c := NewCheck(false, time.Minute)

	got, err := c.NewIssues(t.Context(), h.e.View(), nil, []any{sa, lane, pb})
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.Issue{{Severity: engine.SeverityError, Code: CodePolicyDisabled, Source: engine.Ref{Kind: KindPolicyBinding, ID: pb.Meta.ID}, Target: engine.Ref{Kind: KindPolicy, ID: disabled.Meta.ID}, Field: "spec.policyId"}}
	if diff := cmp.Diff(want, got, ignoreMessage); diff != "" {
		t.Errorf("bound to a disabled policy (-want +got):\n%s", diff)
	}

	empty := &world{}
	proj := empty.project(empty.team())
	h2 := empty.start(t)
	sa2 := &serviceaccount.ServiceAccount{Meta: md("sa-"+seq(), meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}), Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID}}
	lane2 := account.ForServiceAccount(sa2.Meta.ID, sa2.Meta.Name, "", proj.Meta.ID)
	lane2.Meta.ID = meta.NewID()
	if got, err := c.NewIssues(t.Context(), h2.e.View(), nil, []any{sa2, lane2}); err != nil || len(got) != 0 {
		t.Errorf("a lane that reaches no key: %v %v, want nothing refused", got, err)
	}
}

// A check that finds nothing builds once; one that would refuse builds the baseline from the same rows too and refuses only what holds against it.
func TestCheck_ConfirmsRefusalsOnAFreshBaseline(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	c := NewCheck(false, time.Minute)
	var builds atomic.Int32
	c.build = func(src Sources) (*engine.View, error) {
		builds.Add(1)
		return scratchView(src)
	}
	if _, err := c.NewIssues(t.Context(), h.e.View(), nil, []any{newKeyIn(b, b.w.saAccount(b.sa).Meta.ID)}); err != nil || builds.Load() != 1 {
		t.Fatalf("nothing found: %v, %d builds, want 1", err, builds.Load())
	}
	builds.Store(0)
	got, err := c.NewIssues(t.Context(), h.e.View(), nil, []any{newKeyIn(b, meta.NewID())})
	if err != nil || len(got) == 0 || builds.Load() != 2 {
		t.Fatalf("a refusal: %v %v, %d builds, want the refusal after 2", got, err, builds.Load())
	}
}

// Find answers the new warnings from the build the check makes anyway: one build when nothing refuses, and a warning the view already has is not new.
func TestCheck_FindsNewWarningsInOneBuild(t *testing.T) {
	b := newBasic()
	old := b.w.pool(b.hostA)
	old.Spec.AccountIDs = []string{b.acctB.Meta.ID}
	h := b.w.start(t)
	c := NewCheck(false, time.Minute)
	var builds atomic.Int32
	c.build = func(src Sources) (*engine.View, error) {
		builds.Add(1)
		return scratchView(src)
	}
	elsewhere := &pool.Pool{Meta: md("pool-"+seq(), system), Spec: pool.Spec{HostID: b.hostA.Meta.ID, AccountIDs: []string{b.acctB.Meta.ID}}}
	got, err := c.Find(t.Context(), h.e.View(), nil, []any{elsewhere})
	if err != nil || builds.Load() != 1 {
		t.Fatalf("%v, %d builds, want 1", err, builds.Load())
	}
	want := Findings{Warnings: []engine.Issue{{Severity: engine.SeverityWarning, Code: IssueHostMismatch, Source: engine.Ref{Kind: KindPool, ID: elsewhere.Meta.ID}, Target: engine.Ref{Kind: KindAccount, ID: b.acctB.Meta.ID}, Field: "spec.accountIds[0]"}}}
	if diff := cmp.Diff(want, got, ignoreMessage); diff != "" {
		t.Errorf("findings (-want +got):\n%s", diff)
	}
}

// No controller writes to the input rows the scratch build shares with the live view.
func TestCheck_LeavesInputRowsAsTheyWere(t *testing.T) {
	b := newBasic()
	b.w.pool(b.hostA, b.a1, b.a2)
	b.w.bindPolicy(b.w.policy("p-"+seq(), policy.Spec{Models: []string{"acme"}}), b.w.saAccount(b.sa))
	h := b.w.start(t)
	snapshot := func() string {
		v := h.e.View()
		var rows []any
		for _, kind := range []func() []any{
			func() []any { return all[account.Account](v) }, func() []any { return all[pool.Pool](v) },
			func() []any { return all[policy.Policy](v) }, func() []any { return all[policybinding.PolicyBinding](v) },
			func() []any { return all[key.Key](v) }, func() []any { return all[serviceaccount.ServiceAccount](v) },
		} {
			rows = append(rows, kind()...)
		}
		out, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	before := snapshot()
	off := false
	stale := *b.hostA
	stale.Spec.Enabled = &off
	if _, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), []any{&stale}, []any{newKeyIn(b, meta.NewID())}); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(before, snapshot()); diff != "" {
		t.Errorf("live input rows changed (-before +after):\n%s", diff)
	}
}

func all[T any](v *engine.View) []any {
	var out []any
	for r := range engine.All[T](v) {
		out = append(out, r)
	}
	return out
}

func TestCheck_RefusesRowsOfNoInputKind(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	if _, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), nil, []any{&struct{}{}}); err == nil {
		t.Fatal("a row of no input kind was checked")
	}
	if _, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), []any{&struct{}{}}, nil); err != nil {
		t.Fatalf("a read row of no input kind is left out, got %v", err)
	}
}

// Rows the caller read that the live view has not seen yet count as there: a key in a service account and lane written a moment ago is not refused, as it is without them.
func TestCheck_ReadRowsTheViewLacks(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	sa := &serviceaccount.ServiceAccount{Meta: md("sa-"+seq(), meta.Owner{Kind: meta.OwnerProject, ID: b.proj.Meta.ID}), Spec: serviceaccount.Spec{ProjectID: b.proj.Meta.ID}}
	lane := account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, "", b.proj.Meta.ID)
	lane.Meta.ID = meta.NewID()
	k := &key.Key{Meta: md("key-"+seq(), system), Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}, AccountID: lane.Meta.ID, KeyHash: keyHash("fresh")}}
	c := NewCheck(false, time.Minute)

	got, err := c.NewIssues(t.Context(), h.e.View(), []any{sa, lane}, []any{k})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]engine.Issue(nil), got); diff != "" {
		t.Errorf("with the rows read (-want +got):\n%s", diff)
	}
	if got, err := c.NewIssues(t.Context(), h.e.View(), nil, []any{k}); err != nil || len(got) == 0 {
		t.Fatalf("without the rows read: %v %v, want the key refused", got, err)
	}
}

// A row read that is newer than the view and broken: what it breaks among existing rows is not laid on the request, what it breaks among the proposed rows is.
func TestCheck_ReadRowBroken(t *testing.T) {
	b := newBasic()
	existing := b.w.pool(b.hostA, b.a1)
	h := b.w.start(t)
	off := false
	disabled := *b.hostA
	disabled.Spec.Enabled = &off
	ours := &pool.Pool{Meta: md("pool-"+seq(), system), Spec: pool.Spec{HostID: b.hostA.Meta.ID, CredentialIDs: []string{b.a2.Meta.ID}}}

	got, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), []any{&disabled}, []any{ours})
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.Issue{{Severity: engine.SeverityError, Code: IssueHostDisabled, Source: engine.Ref{Kind: KindPool, ID: ours.Meta.ID}, Target: engine.Ref{Kind: KindHost, ID: b.hostA.Meta.ID}, Field: "spec.hostId"}}
	if diff := cmp.Diff(want, got, ignoreMessage); diff != "" {
		t.Errorf("new issues (-want +got):\n%s", diff)
	}
	for _, i := range got {
		if i.Source.ID == existing.Meta.ID {
			t.Errorf("an issue on the existing pool was laid on the request: %+v", i)
		}
	}
}
