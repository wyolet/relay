package stored

import (
	"context"

	"github.com/wyolet/relay/internal/storage/gen"
)

// ErrValueChanged is what a rewrite of a row that changed since it was listed reports.
var ErrValueChanged = errValueChanged

// StaleRows and RewriteRow split a rewrite pass where another write can land between its listing and its write.
func (s *Backend) StaleRows(ctx context.Context) ([]gen.ListStaleSecretValuesRow, error) {
	return s.q.ListStaleSecretValues(ctx, gen.ListStaleSecretValuesParams{After: "", MaxRows: rewriteBatch})
}

func (s *Backend) RewriteRow(ctx context.Context, r gen.ListStaleSecretValuesRow) error {
	ring, err := s.keys.load(ctx)
	if err != nil {
		return err
	}
	return s.rewriteRow(ctx, ring, r)
}

// RewriterWoken reports, and clears, a pending wake-up of the rewriter.
func (s *Backend) RewriterWoken() bool {
	select {
	case <-s.keys.wake:
		return true
	default:
		return false
	}
}
