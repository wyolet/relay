package pseudo

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	appcatalog "github.com/wyolet/relay/app/catalog"
)

// steps records the order outside writes, their undos and the transaction run in.
type steps struct{ log []string }

func (s *steps) outside(what string, writeErr, undoErr error) Outside {
	return Outside{
		What:  what,
		Write: func(context.Context) error { s.log = append(s.log, "write "+what); return writeErr },
		Undo:  func(context.Context) error { s.log = append(s.log, "undo "+what); return undoErr },
	}
}

func (s *steps) expand(rows ...Row) ExpandFunc {
	return func(context.Context, *appcatalog.Stores) ([]Row, error) {
		s.log = append(s.log, "transaction")
		return rows, nil
	}
}

// Outside writes are made in order before the transaction and kept when it commits.
func TestWriter_OutsideBeforeTheTransaction(t *testing.T) {
	fw := newFrameWorld(t)
	s := &steps{}
	tx := &fakeTx{}
	w := fw.writer(tx)
	w.Outside = []Outside{s.outside("a", nil, nil), s.outside("b", nil, nil)}
	if _, err := w.Run(t.Context(), s.expand(fw.good), createAll, false); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"write a", "write b", "transaction"}, s.log); diff != "" || !tx.committed() {
		t.Errorf("steps (-want +got):\n%s; committed %v", diff, tx.committed())
	}
}

// A dry run makes no outside write and so undoes none.
func TestWriter_OutsideNotOnDryRun(t *testing.T) {
	fw := newFrameWorld(t)
	s := &steps{}
	w := fw.writer(&fakeTx{})
	w.Outside = []Outside{s.outside("a", nil, nil)}
	if _, err := w.Run(t.Context(), s.expand(fw.good), createAll, true); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"transaction"}, s.log); diff != "" {
		t.Errorf("steps (-want +got):\n%s", diff)
	}
}

// A transaction that does not commit undoes the outside writes, the last first, and answers its own error; an undo that fails is reported with what it left behind.
func TestWriter_OutsideUndone(t *testing.T) {
	refused := errors.New("forbidden")
	stuck := errors.New("connection reset")
	create := func(context.Context, *appcatalog.Stores, Row) error { return refused }
	for _, tc := range []struct {
		name     string
		undoErr  error
		wantLeft []string
	}{
		{"undone", nil, nil},
		{"undo fails", stuck, []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fw := newFrameWorld(t)
			s := &steps{}
			w := fw.writer(&fakeTx{})
			w.Outside = []Outside{s.outside("a", nil, tc.undoErr), s.outside("b", nil, nil)}
			_, err := w.Run(t.Context(), s.expand(fw.good), create, false)
			if diff := cmp.Diff([]string{"write a", "write b", "transaction", "undo b", "undo a"}, s.log); diff != "" {
				t.Errorf("steps (-want +got):\n%s", diff)
			}
			var ue *UndoError
			switch {
			case tc.wantLeft == nil:
				var re *RowError
				if !errors.As(err, &re) || errors.As(err, &ue) {
					t.Fatalf("error = %v, want the row refusal alone", err)
				}
			case !errors.As(err, &ue):
				t.Fatalf("error = %v, want an UndoError", err)
			default:
				var re *RowError
				if diff := cmp.Diff(tc.wantLeft, ue.Left); diff != "" || !errors.As(ue.Err, &re) || !errors.Is(ue.Undo, stuck) {
					t.Errorf("left (-want +got):\n%s; err %v, undo %v", diff, ue.Err, ue.Undo)
				}
			}
		})
	}
}

// An outside write that fails opens no transaction and undoes the ones made before it.
func TestWriter_OutsideWriteFails(t *testing.T) {
	fw := newFrameWorld(t)
	s := &steps{}
	tx := &fakeTx{}
	failed := errors.New("no master key")
	w := fw.writer(tx)
	w.Outside = []Outside{s.outside("a", nil, nil), s.outside("b", failed, nil)}
	_, err := w.Run(t.Context(), s.expand(fw.good), createAll, false)
	if !errors.Is(err, failed) || len(tx.answered) != 0 {
		t.Fatalf("error = %v, transactions %v; want the write's error before any transaction", err, tx.answered)
	}
	if diff := cmp.Diff([]string{"write a", "write b", "undo a"}, s.log); diff != "" {
		t.Errorf("steps (-want +got):\n%s", diff)
	}
}
