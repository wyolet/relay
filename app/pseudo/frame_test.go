package pseudo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/wyolet/relay/app/account"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// fakeTx runs fn outside any database and remembers what fn answered: nil commits, anything else rolls back.
type fakeTx struct{ answered []error }

func (f *fakeTx) InTx(ctx context.Context, fn func(ctx context.Context, tx *appcatalog.Stores) error) error {
	err := fn(ctx, &appcatalog.Stores{})
	f.answered = append(f.answered, err)
	return err
}

func (f *fakeTx) committed() bool { return len(f.answered) == 1 && f.answered[0] == nil }

// frameWorld is a project with a service account and its lane, and two keys of it the frame may write: one in the lane, one in an account that does not exist.
type frameWorld struct {
	plans     *plantest.Plans
	good, bad Row
}

func newFrameWorld(t *testing.T) *frameWorld {
	w := &plantest.World{}
	sa := w.ServiceAccount(w.Project(w.Team()))
	fw := &frameWorld{plans: w.Load(t)}
	mk := func(accountID string) Row {
		k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "k-" + meta.NewID()}, Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}, AccountID: accountID, KeyHash: meta.NewID()}}
		return Row{Kind: "key", Pointer: "/spec/key", Meta: &k.Meta, Value: k}
	}
	fw.good, fw.bad = mk(w.ServiceAccountAccount(sa).Meta.ID), mk(meta.NewID())
	return fw
}

func (fw *frameWorld) writer(tx *fakeTx) Writer {
	return Writer{Stores: tx, Check: plan.NewCheck(false, time.Minute), View: fw.plans.Engine.View}
}

func expandTo(rows ...Row) ExpandFunc {
	return func(context.Context, *appcatalog.Stores) ([]Row, error) { return rows, nil }
}

func createAll(context.Context, *appcatalog.Stores, Row) error { return nil }

func TestWriter_CommitsAndDryRuns(t *testing.T) {
	fw := newFrameWorld(t)
	for _, dry := range []bool{false, true} {
		tx := &fakeTx{}
		rows, err := fw.writer(tx).Run(t.Context(), expandTo(fw.good), createAll, dry)
		if err != nil {
			t.Fatalf("dry run %v: %v", dry, err)
		}
		if diff := cmp.Diff([]Row{fw.good}, rows); diff != "" {
			t.Errorf("dry run %v: rows (-want +got):\n%s", dry, diff)
		}
		if tx.committed() == dry {
			t.Errorf("dry run %v: transaction answered %v", dry, tx.answered)
		}
	}
}

// A refused row stops the run with its row named, and nothing after it is created.
func TestWriter_RowRefused(t *testing.T) {
	fw := newFrameWorld(t)
	tx := &fakeTx{}
	refused := errors.New("forbidden")
	var created []string
	create := func(_ context.Context, _ *appcatalog.Stores, r Row) error {
		created = append(created, r.Meta.ID)
		if r.Meta.ID == fw.good.Meta.ID {
			return refused
		}
		return nil
	}
	_, err := fw.writer(tx).Run(t.Context(), expandTo(fw.good, fw.bad), create, false)
	var re *RowError
	if !errors.As(err, &re) || re.Row.Meta.ID != fw.good.Meta.ID || !errors.Is(err, refused) {
		t.Fatalf("error = %v, want the first row refused", err)
	}
	if diff := cmp.Diff([]string{fw.good.Meta.ID}, created); diff != "" || tx.committed() {
		t.Errorf("created (-want +got):\n%s; committed %v", diff, tx.committed())
	}
}

// Rows that pass one by one but raise a plan error together are refused with the issues, and nothing commits.
func TestWriter_PlanIssues(t *testing.T) {
	fw := newFrameWorld(t)
	tx := &fakeTx{}
	_, err := fw.writer(tx).Run(t.Context(), expandTo(fw.good, fw.bad), createAll, false)
	var ie *IssuesError
	if !errors.As(err, &ie) {
		t.Fatalf("error = %v, want plan issues", err)
	}
	want := []engine.Issue{{Severity: engine.SeverityError, Code: plan.IssueRefMissing, Source: engine.Ref{Kind: plan.KindKey, ID: fw.bad.Meta.ID}, Target: engine.Ref{Kind: plan.KindAccount, ID: fw.bad.Value.(*key.Key).Spec.AccountID}, Field: "spec.accountId"}}
	if diff := cmp.Diff(want, ie.Issues, cmpopts.IgnoreFields(engine.Issue{}, "Message")); diff != "" || tx.committed() {
		t.Errorf("issues (-want +got):\n%s; committed %v", diff, tx.committed())
	}
}

// Rows expand and create record are laid over the live view: a key in a service account and lane the view has not seen yet passes, and fails without them. Recording outside a run does nothing.
func TestWriter_LaysRecordedReads(t *testing.T) {
	w := &plantest.World{}
	proj := w.Project(w.Team())
	plans := w.Load(t)
	owner := meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "fresh", Owner: owner}, Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID}}
	lane := account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, "", owner)
	lane.Meta.ID = meta.NewID()
	k := &key.Key{Meta: meta.Metadata{ID: meta.NewID(), Name: "fresh-key"}, Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}, AccountID: lane.Meta.ID, KeyHash: meta.NewID()}}
	row := Row{Kind: "key", Pointer: "/spec/key", Meta: &k.Meta, Value: k}
	Record(t.Context(), sa.Meta.ID, sa)
	writer := Writer{Stores: &fakeTx{}, Check: plan.NewCheck(false, time.Minute), View: plans.Engine.View}

	expand := func(record bool) ExpandFunc {
		return func(ctx context.Context, _ *appcatalog.Stores) ([]Row, error) {
			if record {
				Record(ctx, sa.Meta.ID, sa)
				Record(ctx, lane.Meta.ID, lane)
			}
			return []Row{row}, nil
		}
	}
	if _, err := writer.Run(t.Context(), expand(true), createAll, false); err != nil {
		t.Fatalf("with the reads recorded: %v", err)
	}
	var ie *IssuesError
	if _, err := writer.Run(t.Context(), expand(false), createAll, false); !errors.As(err, &ie) {
		t.Fatalf("without them: %v, want plan issues", err)
	}
}

// A deleted row is gone from what the plans are checked against: a key made in a lane the same write deletes names an account that is not there.
func TestWriter_DeleteStep(t *testing.T) {
	fw := newFrameWorld(t)
	acc := engine.Get[account.Account](fw.plans.Engine.View(), fw.good.Value.(*key.Key).Spec.AccountID)
	gone := Row{Kind: "account", Pointer: "/spec", Meta: &acc.Meta, Value: acc, Op: OpDelete}
	tx := &fakeTx{}
	_, err := fw.writer(tx).Run(t.Context(), expandTo(fw.good, gone), createAll, false)
	var ie *IssuesError
	if !errors.As(err, &ie) || tx.committed() {
		t.Fatalf("error = %v, committed %v; want plan issues", err, tx.committed())
	}
	want := []engine.Issue{{Severity: engine.SeverityError, Code: plan.IssueRefMissing, Source: engine.Ref{Kind: plan.KindKey, ID: fw.good.Meta.ID}, Target: engine.Ref{Kind: plan.KindAccount, ID: acc.Meta.ID}, Field: "spec.accountId"}}
	if diff := cmp.Diff(want, ie.Issues, cmpopts.IgnoreFields(engine.Issue{}, "Message")); diff != "" {
		t.Errorf("issues (-want +got):\n%s", diff)
	}
}

// The hook on ctx sees the plans with the steps applied once the check passed; its refusal stops the write.
func TestWriter_AfterCheck(t *testing.T) {
	fw := newFrameWorld(t)
	var seen bool
	ctx := AfterCheck(t.Context(), func(f plan.Findings) error {
		seen = engine.Get[key.Key](f.Result, fw.good.Meta.ID) != nil
		return nil
	})
	tx := &fakeTx{}
	if _, err := fw.writer(tx).Run(ctx, expandTo(fw.good), createAll, false); err != nil || !seen || !tx.committed() {
		t.Fatalf("error %v, key seen %v, committed %v", err, seen, tx.committed())
	}
	refused := errors.New("no")
	ctx = AfterCheck(t.Context(), func(plan.Findings) error { return refused })
	tx = &fakeTx{}
	if _, err := fw.writer(tx).Run(ctx, expandTo(fw.good), createAll, false); !errors.Is(err, refused) || tx.committed() {
		t.Fatalf("error %v, committed %v; want the hook's refusal", err, tx.committed())
	}
}

// A row a step creates is left out of the reads; of one a step updates or deletes only the first read stays, the row as stored before the write.
func TestReads_Except(t *testing.T) {
	ctx, r := withReads(t.Context())
	before := &key.Key{Meta: meta.Metadata{ID: "changed", Name: "before"}}
	after := &key.Key{Meta: meta.Metadata{ID: "changed", Name: "after"}}
	other := &key.Key{Meta: meta.Metadata{ID: "other"}}
	made := &key.Key{Meta: meta.Metadata{ID: "made"}}
	for _, k := range []*key.Key{before, made, other, after, other} {
		Record(ctx, k.Meta.ID, k)
	}
	got := r.except(map[string]bool{"made": true}, map[string]bool{"changed": true})
	if diff := cmp.Diff([]any{before, other, other}, got); diff != "" {
		t.Errorf("reads (-want +got):\n%s", diff)
	}
}

// A writer that waits for the check slot holds no transaction: past the bound it answers ErrCheckTimeout without having opened one; once the slot frees, it runs.
func TestWriter_WaitsForTheSlotBeforeItsTransaction(t *testing.T) {
	fw := newFrameWorld(t)
	tx := &fakeTx{}
	w := fw.writer(tx)
	w.Check = plan.NewCheck(false, 30*time.Millisecond)
	_, release, err := w.Check.Hold(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run(t.Context(), expandTo(fw.good), createAll, false); !errors.Is(err, plan.ErrCheckTimeout) || len(tx.answered) != 0 {
		t.Fatalf("while the slot is held: %v, transactions %v; want ErrCheckTimeout and none opened", err, tx.answered)
	}
	release()
	if _, err := w.Run(t.Context(), expandTo(fw.good), createAll, false); err != nil || !tx.committed() {
		t.Fatalf("once the slot is free: %v, transactions %v", err, tx.answered)
	}
}

// The warnings answered are those new on the rows written; a warning an untouched row already has is not listed, and a clean write answers none.
func TestWriter_Warnings(t *testing.T) {
	w := &plantest.World{}
	hostA, hostB := w.Host("a", "http://a.invalid"), w.Host("b", "http://b.invalid")
	atB := w.Account(hostB)
	w.Credential(hostB, atB, plantest.System, "sk-b")
	old := w.Pool(hostA)
	old.Spec.AccountIDs = []string{atB.Meta.ID}
	plans := w.Load(t)
	writer := Writer{Stores: &fakeTx{}, Check: plan.NewCheck(false, time.Minute), View: plans.Engine.View}
	ours := &pool.Pool{Meta: meta.Metadata{ID: meta.NewID(), Name: "ours"}, Spec: pool.Spec{HostID: hostA.Meta.ID, AccountIDs: []string{atB.Meta.ID}}}
	row := Row{Kind: "pool", Pointer: "/spec/access/accountIds", Meta: &ours.Meta, Value: ours}

	_, got, err := writer.RunWithWarnings(t.Context(), expandTo(row), createAll, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []engine.Issue{{Severity: engine.SeverityWarning, Code: plan.IssueHostMismatch, Source: engine.Ref{Kind: plan.KindPool, ID: ours.Meta.ID}, Target: engine.Ref{Kind: plan.KindAccount, ID: atB.Meta.ID}, Field: "spec.accountIds[0]"}}
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(engine.Issue{}, "Message")); diff != "" {
		t.Errorf("warnings (-want +got):\n%s", diff)
	}

	clean := &pool.Pool{Meta: meta.Metadata{ID: meta.NewID(), Name: "clean"}, Spec: pool.Spec{HostID: hostB.Meta.ID, AccountIDs: []string{atB.Meta.ID}}}
	if _, got, err := writer.RunWithWarnings(t.Context(), expandTo(Row{Kind: "pool", Meta: &clean.Meta, Value: clean}), createAll, true); err != nil || len(got) != 0 {
		t.Errorf("a clean write: %v %v, want no warning", got, err)
	}
}

func TestWriter_NoPlans(t *testing.T) {
	tx := &fakeTx{}
	w := Writer{Stores: tx, Check: plan.NewCheck(false, time.Minute), View: func() *engine.View { return nil }}
	if _, err := w.Run(t.Context(), expandTo(), createAll, false); !errors.Is(err, ErrNoPlans) || len(tx.answered) != 0 {
		t.Fatalf("error = %v, transactions %v; want ErrNoPlans before any transaction", err, tx.answered)
	}
}

func TestWriter_BodyRefused(t *testing.T) {
	fw := newFrameWorld(t)
	tx := &fakeTx{}
	bad := &BodyError{Pointer: "/spec", Message: "no"}
	expand := func(context.Context, *appcatalog.Stores) ([]Row, error) { return nil, bad }
	if _, err := fw.writer(tx).Run(t.Context(), expand, createAll, false); !errors.Is(err, bad) || tx.committed() {
		t.Fatalf("error = %v, committed %v", err, tx.committed())
	}
}
