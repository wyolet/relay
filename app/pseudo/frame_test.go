package pseudo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
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
