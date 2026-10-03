package jobq

import (
	"context"
	"time"
)

func (q *Queue) scheduleLoop(ctx context.Context) {
	t := time.NewTicker(q.opts.ScheduleInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if _, err := q.store.promoteDue(fctx); err != nil {
				q.opts.Logger.Warn("jobq: promote failed", "err", err)
			}
			cancel()
		}
	}
}

func (q *Queue) rescueLoop(ctx context.Context) {
	t := time.NewTicker(q.opts.RescueInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			horizon := time.Now().Add(-q.opts.RescueAfter)
			if _, err := q.store.rescueStuck(fctx, horizon, 100); err != nil {
				q.opts.Logger.Warn("jobq: rescue failed", "err", err)
			}
			cancel()
		}
	}
}
