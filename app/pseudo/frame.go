// Package pseudo turns a pseudo object, a request shape that stands for several real rows, into those rows and writes them together. A pseudo object has no table, no id and no read: the real rows are what is stored, checked and audited. Each kind's expansion is a pure function from its body and what it read to the rows in write order, each with the JSON pointer of the part of the body it came from; Writer runs such a list in one transaction, each row through the caller's create function, then checks the whole set against the plans before the commit.
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

// Row is one real row a pseudo object expands to.
type Row struct {
	// Kind is the API singular of the row's kind, such as service-account or policy-binding.
	Kind string
	// Pointer is the JSON pointer of the part of the pseudo body the row came from.
	Pointer string
	// Meta is the metadata inside Value.
	Meta *meta.Metadata
	// Value points at the row, such as a *pool.Pool; the create function writes it as it leaves it.
	Value any
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

// ExpandFunc reads what a kind's expansion needs through tx and returns the rows in write order.
type ExpandFunc func(ctx context.Context, tx *appcatalog.Stores) ([]Row, error)

// CreateFunc runs the checks row's own create route runs and writes it through tx.
type CreateFunc func(ctx context.Context, tx *appcatalog.Stores, row Row) error

// Writer writes expanded rows in one transaction once the plans accept them.
type Writer struct {
	Stores Transactor
	Check  *plan.Check
	// View is the live plans' current view.
	View func() *engine.View
	// Outside are the writes made before the transaction opens, undone when it does not commit.
	Outside []Outside
}

// Run takes the check slot (plan.Check.Hold) before its outside writes and transaction begin and frees it once the transaction ended and what it did not commit was undone, so one write is open at a time and a writer that waits holds no connection; one that waits past the bound answers plan.ErrCheckTimeout. It makes the outside writes, then expands, creates each row in order, checks the rows against the plans and commits; a dry run does all of it but the outside writes and the commit. It returns the rows as created, also on a dry run. A refused row is a *RowError, a body that expands to nothing a *BodyError, rows the plans refuse an *IssuesError; nothing is written on any error, and an *UndoError names what an outside write left behind when it could not be undone. The rows expand and create read are those they pass to Record under the ctx they are given; the plan check lays them over the live view, under the rows created.
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
	written, err := w.writeOutside(ctx, dryRun)
	if err != nil {
		return nil, err
	}
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
		ours := make(map[string]bool, len(rows))
		for _, r := range rows {
			ours[r.Meta.ID] = true
		}
		issues, err := w.Check.NewIssues(ctx, live, read.except(ours), Values(rows))
		if err != nil {
			return err
		}
		if len(issues) > 0 {
			return &IssuesError{Rows: rows, Issues: issues}
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
		return nil, undoOutside(ctx, written, err)
	}
	return rows, nil
}

// Values is the rows' values as the plans see them, in order: an AccountRebinding is the binding it made.
func Values(rows []Row) []any {
	out := make([]any, len(rows))
	for i, r := range rows {
		out[i] = r.Value
		if rb, ok := r.Value.(*AccountRebinding); ok {
			out[i] = &rb.Binding
		}
	}
	return out
}
