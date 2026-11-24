package pseudo

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Outside is a write a pseudo kind makes before its rows' transaction opens, on a connection that transaction cannot share, such as a stored secret's value (see app/catalog.Stores.InTx). Writer.Run makes each one in order once the plans are loaded, never on a dry run, and undoes those it made when the transaction does not commit.
type Outside struct {
	// What names what Write makes, for errors; it never carries a secret.
	What string
	// Write makes it; a Write that fails leaves nothing.
	Write func(ctx context.Context) error
	// Undo removes what Write made. It runs without the request's cancellation, and leaves alone a write the transaction committed after all.
	Undo func(ctx context.Context) error
}

// UndoError is a pseudo write that wrote no row but could not undo every write it made before its transaction: Left names each one left behind, Err is why no row was written.
type UndoError struct {
	Err  error
	Left []string
	Undo error
}

func (e *UndoError) Error() string {
	return fmt.Sprintf("%v; no row was written, but undoing what was written before the transaction failed, leaving %s behind: %v", e.Err, strings.Join(e.Left, ", "), e.Undo)
}

// writeOutside makes w's outside writes in order and returns those made; when one fails, the ones made before it are undone. A dry run makes none.
func (w Writer) writeOutside(ctx context.Context, dryRun bool) ([]Outside, error) {
	if dryRun {
		return nil, nil
	}
	var done []Outside
	for _, o := range w.Outside {
		if err := o.Write(ctx); err != nil {
			return nil, undoOutside(ctx, done, fmt.Errorf("pseudo: writing %s: %w", o.What, err))
		}
		done = append(done, o)
	}
	return done, nil
}

// undoOutside undoes done, the last first, once err kept the rows from being written; it returns err, or an *UndoError when something is left behind.
func undoOutside(ctx context.Context, done []Outside, err error) error {
	ctx = context.WithoutCancel(ctx)
	ue := &UndoError{Err: err}
	for i := len(done) - 1; i >= 0; i-- {
		if uerr := done[i].Undo(ctx); uerr != nil {
			ue.Left = append(ue.Left, done[i].What)
			ue.Undo = errors.Join(ue.Undo, uerr)
		}
	}
	if len(ue.Left) == 0 {
		return err
	}
	return ue
}
