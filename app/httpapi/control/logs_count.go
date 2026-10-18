package control

import (
	"context"

	"golang.org/x/sync/errgroup"

	"github.com/wyolet/relay/app/usagelog"
)

// logsTotalCap bounds the /logs?count=true total so a wide window never costs a full scan of the store.
const logsTotalCap = 10_000

// fetchLogs reads one page of q and, with withCount, the number of events matching q across all pages, counted concurrently up to logsTotalCap+1.
func fetchLogs(ctx context.Context, r usagelog.Reader, q usagelog.EventQuery, withCount bool) ([]usagelog.Event, int64, error) {
	if !withCount {
		events, err := r.Events(ctx, q)
		return events, 0, err
	}
	var (
		events []usagelog.Event
		total  int64
	)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		events, err = r.Events(gctx, q)
		return err
	})
	g.Go(func() (err error) {
		total, err = r.Count(gctx, q, logsTotalCap+1)
		return err
	})
	if err := g.Wait(); err != nil {
		return nil, 0, err
	}
	return events, total, nil
}

// cappedTotal maps a count taken with limit logsTotalCap+1 to the response's total and total_capped.
func cappedTotal(n int64) (*int64, *bool) {
	capped := n > logsTotalCap
	if capped {
		n = logsTotalCap
	}
	return &n, &capped
}
