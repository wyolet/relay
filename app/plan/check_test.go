package plan

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/pool"
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

	got, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), []any{k, elsewhere})
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

	got, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), []any{&same, fine})
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

	got, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), []any{twin})
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
		if _, err := c.NewIssues(t.Context(), h.e.View(), []any{newKeyIn(b, meta.NewID())}); err != nil {
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
	if _, err := c.NewIssues(t.Context(), h.e.View(), nil); !errors.Is(err, ErrCheckTimeout) {
		t.Fatalf("while another check runs: %v, want ErrCheckTimeout", err)
	}
	<-c.slot
	c.timeout = time.Minute
	if _, err := c.NewIssues(t.Context(), h.e.View(), nil); err != nil {
		t.Fatalf("once the slot is free: %v", err)
	}
}

func TestCheck_RefusesRowsOfNoInputKind(t *testing.T) {
	b := newBasic()
	h := b.w.start(t)
	if _, err := NewCheck(false, time.Minute).NewIssues(t.Context(), h.e.View(), []any{&struct{}{}}); err == nil {
		t.Fatal("a row of no input kind was checked")
	}
}
