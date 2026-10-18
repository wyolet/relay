package usage

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// SummaryPrevious is one summary row's numbers over the previous window, with the same meaning as the SummaryRow fields of the same name.
type SummaryPrevious struct {
	Requests   int64            `json:"requests"`
	ErrorCount int64            `json:"error_count"`
	Errors4xx  int64            `json:"errors_4xx"`
	Errors5xx  int64            `json:"errors_5xx"`
	Throttled  int64            `json:"throttled"`
	Tokens     map[string]int64 `json:"tokens"`
	CostNanos  int64            `json:"cost_nanos"`
	Unpriced   int64            `json:"unpriced"`
	DurationMs DurationStats    `json:"duration_ms"`
	TTFTMs     *DurationStats   `json:"ttft_ms,omitempty"`
}

// ErrNoPreviousWindow rejects a comparison whose window has no lower bound or ends before it starts: neither has a previous window of equal length.
var ErrNoPreviousWindow = errors.New("usage: comparing with the previous window needs a window with a start that is not after its end")

// ResolveWindow returns q's absolute bounds as every backend applies them: the lower bound is From, else now-Since; the upper bound is To, else now.
func ResolveWindow(q EventQuery, now time.Time) (from, to time.Time, err error) {
	from, to = q.From, q.To
	if to.IsZero() {
		to = now
	}
	if from.IsZero() {
		if q.Since <= 0 {
			return time.Time{}, time.Time{}, ErrNoPreviousWindow
		}
		from = now.Add(-q.Since)
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, ErrNoPreviousWindow
	}
	return from, to, nil
}

// PreviousWindow is the window of the same length as [from, to] that ends at from. The returned end is exclusive, so no event falls in both windows.
func PreviousWindow(from, to time.Time) (time.Time, time.Time) {
	return from.Add(-to.Sub(from)), from
}

// SummaryWithPrecedingWindow is SummaryWithPrevious over the window of equal length right before q's. q's window is resolved once and both queries use it, so a relative Since cannot drift between them.
func SummaryWithPrecedingWindow(ctx context.Context, r Reader, q SummaryQuery) (SummaryResult, error) {
	from, to, err := ResolveWindow(q.EventQuery, time.Now())
	if err != nil {
		return SummaryResult{}, err
	}
	q.Since, q.From, q.To = 0, from, to
	prevFrom, prevTo := PreviousWindow(from, to)
	return SummaryWithPrevious(ctx, r, q, prevFrom, prevTo)
}

// SummaryWithPrevious runs q, and q's filters over [prevFrom, prevTo) concurrently, and attaches each current row's previous numbers by group. The previous window may have any length and may overlap q's: the caller picks it.
func SummaryWithPrevious(ctx context.Context, r Reader, q SummaryQuery, prevFrom, prevTo time.Time) (SummaryResult, error) {
	prev := q
	prev.Since, prev.From, prev.To, prev.Before = 0, prevFrom, time.Time{}, prevTo

	var res, prevRes SummaryResult
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() (err error) {
		res, err = r.Summary(gctx, q)
		return err
	})
	g.Go(func() (err error) {
		prevRes, err = r.Summary(gctx, prev)
		return err
	})
	if err := g.Wait(); err != nil {
		return SummaryResult{}, err
	}

	res.Rows = attachPrevious(res.Rows, prevRes.Rows)
	res.PreviousFrom, res.PreviousTo = prevFrom, prevTo
	return res, nil
}

// attachPrevious sets Previous on each current row that has a previous row with the same group. Previous-only groups are dropped: the rows describe the current window.
func attachPrevious(rows, prevRows []SummaryRow) []SummaryRow {
	byGroup := make(map[string]*SummaryRow, len(prevRows))
	for i := range prevRows {
		byGroup[groupIdentity(prevRows[i].Group)] = &prevRows[i]
	}
	for i := range rows {
		p, ok := byGroup[groupIdentity(rows[i].Group)]
		if !ok {
			continue
		}
		rows[i].Previous = &SummaryPrevious{
			Requests:   p.Requests,
			ErrorCount: p.ErrorCount,
			Errors4xx:  p.Errors4xx,
			Errors5xx:  p.Errors5xx,
			Throttled:  p.Throttled,
			Tokens:     p.Tokens,
			CostNanos:  p.CostNanos,
			Unpriced:   p.Unpriced,
			DurationMs: p.DurationMs,
			TTFTMs:     p.TTFTMs,
		}
	}
	return rows
}

// groupIdentity encodes a Group map as a comparable key; NUL separators keep distinct maps from colliding.
func groupIdentity(group map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(group)) {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(group[k])
		b.WriteByte(0)
	}
	return b.String()
}
