// Package pseudo turns a pseudo object, a request shape that stands for several real rows, into those rows and writes them together. A pseudo object has no table, no id and no read: the real rows are what is stored, checked and audited. Each kind's expansion is a pure function from its body and what it read to the steps in write order, each a row to create, update or delete with the JSON pointer of the part of the body it came from; Writer runs such a list in one transaction, each step through the caller's write function, then checks the whole result against the plans before the commit.
//
// Out of scope: the checks one row passes (the caller's create function runs the ones the row's own create route runs), HTTP, and audit.
package pseudo

import (
	"context"
	"errors"
	"fmt"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// Op is what a step does to its row.
type Op string

const (
	// OpCreate makes the row; the zero Op.
	OpCreate Op = ""
	// OpUpdate replaces the stored row of the same id with the row, whose resource version is the one read in the transaction.
	OpUpdate Op = "update"
	// OpDelete deletes the stored row; the row is the one read in the transaction.
	OpDelete Op = "delete"
)

// Row is one step of a pseudo object: a real row to create, update or delete.
type Row struct {
	// Kind is the API singular of the row's kind, such as service-account or policy-binding.
	Kind string
	// Pointer is the JSON pointer of the part of the pseudo body the row came from.
	Pointer string
	// Meta is the metadata inside Value.
	Meta *meta.Metadata
	// Value points at the row, such as a *pool.Pool; the write function writes it as it leaves it.
	Value any
	// Op is what the step does to the row.
	Op Op
}

// BodyError is a pseudo body that expands to no rows; Pointer names the part at fault.
type BodyError struct {
	Pointer string
	Message string
}

func (e *BodyError) Error() string { return e.Message }

// RowError is a row its create function refused or failed to write.
type RowError struct {
	Row Row
	Err error
}

func (e *RowError) Error() string { return fmt.Sprintf("%s %s: %v", e.Row.Kind, e.Row.Pointer, e.Err) }
func (e *RowError) Unwrap() error { return e.Err }

// IssuesError is a set of rows the plan check refused: Issues are the errors the rows would raise that the live plans do not have.
type IssuesError struct {
	Rows   []Row
	Issues []engine.Issue
}

func (e *IssuesError) Error() string {
	return fmt.Sprintf("the rows would raise %d plan issues", len(e.Issues))
}

// ErrNoPlans is a write with no published plans to check it against.
var ErrNoPlans = errors.New("pseudo: no published plans to check against")

// errDryRun rolls back a dry run's transaction.
var errDryRun = errors.New("pseudo: dry run")

// Transactor runs fn with the stores bound to one transaction; *catalog.Stores is one.
type Transactor interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx *appcatalog.Stores) error) error
}

// ExpandFunc reads what a kind's expansion needs through tx and returns the steps in write order.
type ExpandFunc func(ctx context.Context, tx *appcatalog.Stores) ([]Row, error)

// CreateFunc runs the checks the route of row's step runs (its kind's create, update or delete, by row.Op) and writes it through tx.
type CreateFunc func(ctx context.Context, tx *appcatalog.Stores, row Row) error

// Writer writes expanded rows in one transaction once the plans accept them.
type Writer struct {
	Stores Transactor
	Check  *plan.Check
	// View is the live plans' current view.
	View func() *engine.View
}

// Run takes the check slot (plan.Check.Hold) before its transaction begins and frees it once the transaction ended, so one write is open at a time and a writer that waits holds no connection; one that waits past the bound answers plan.ErrCheckTimeout. It expands, writes each step in order, checks the result against the plans and commits; a dry run does all of it but the commit. It returns the steps as written, also on a dry run. A refused step is a *RowError, a body that expands to nothing a *BodyError, steps the plans refuse an *IssuesError; nothing is written on any error. The rows expand and create read are those they pass to Record under the ctx they are given; the plan check lays them over the live view, under the rows created and updated and without the rows deleted. A row a step updates or deletes is laid in the baseline as first read, before any step wrote it. Once the check passed, the hook AfterCheck put on ctx is given the result.
func (w Writer) Run(ctx context.Context, expand ExpandFunc, create CreateFunc, dryRun bool) ([]Row, error) {
	if w.Check == nil || w.View == nil {
		return nil, ErrNoPlans
	}
	live := w.View()
	if live == nil || live.Gen() == 0 {
		return nil, ErrNoPlans
	}
	ctx, release, err := w.Check.Hold(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, read := withReads(ctx)
	var rows []Row
	err = w.Stores.InTx(ctx, func(ctx context.Context, tx *appcatalog.Stores) error {
		var err error
		if rows, err = expand(ctx, tx); err != nil {
			return err
		}
		for _, r := range rows {
			if err := create(ctx, tx, r); err != nil {
				return &RowError{Row: r, Err: err}
			}
		}
		created, changed := map[string]bool{}, map[string]bool{}
		for _, r := range rows {
			if r.Op == OpCreate {
				created[r.Meta.ID] = true
			} else {
				changed[r.Meta.ID] = true
			}
		}
		issues, result, err := w.Check.NewIssuesAndView(ctx, live, read.except(created, changed), proposal(rows))
		if err != nil {
			return err
		}
		if len(issues) > 0 {
			return &IssuesError{Rows: rows, Issues: issues}
		}
		if after, _ := ctx.Value(afterCheckKey{}).(func(*engine.View) error); after != nil {
			if err := after(result); err != nil {
				return err
			}
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		return rows, nil
	}
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// Values is the rows' values, in order.
func Values(rows []Row) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r.Value
	}
	return out
}

// proposal is what the steps propose to the plan check: each created or updated row, and each deleted one as a plan.Deleted.
func proposal(rows []Row) []any {
	out := Values(rows)
	for i, r := range rows {
		if r.Op == OpDelete {
			out[i] = plan.Deleted{Row: r.Value}
		}
	}
	return out
}

type afterCheckKey struct{}

// AfterCheck returns ctx carrying fn for the pseudo write run under it: once the plan check passed, fn is given the view of the plans with the steps applied, before the commit; an error from fn refuses the write. A kind reads there what its rows make of the plans.
func AfterCheck(ctx context.Context, fn func(*engine.View) error) context.Context {
	return context.WithValue(ctx, afterCheckKey{}, fn)
}
