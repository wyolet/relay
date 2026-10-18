package control

import (
	"context"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/usage"
)

// comparePrevious is the only accepted `compare` value. It is checked here rather than with a huma enum so a bad value is a 400 like every other usage filter.
const comparePrevious = "previous"

func parseCompare(raw string) (bool, error) {
	switch raw {
	case "":
		return false, nil
	case comparePrevious:
		return true, nil
	default:
		return false, huma.Error400BadRequest("`compare` must be \"previous\"")
	}
}

// readSummary runs q, attaching each row's previous-window numbers when compare is set.
func readSummary(ctx context.Context, r usagelog.Reader, q usagelog.SummaryQuery, compare bool) (usagelog.SummaryResult, error) {
	if !compare {
		return r.Summary(ctx, q)
	}
	res, err := usage.SummaryWithPrevious(ctx, r, q)
	if errors.Is(err, usage.ErrNoPreviousWindow) {
		return res, huma.Error400BadRequest(err.Error())
	}
	if err != nil {
		return res, err
	}
	// The window the previous one was cut from, for a result with no events to derive from/to from.
	from, to := res.PreviousTo, res.PreviousTo.Add(res.PreviousTo.Sub(res.PreviousFrom))
	if res.From.IsZero() {
		res.From = from
	}
	if res.To.IsZero() {
		res.To = to
	}
	return res, nil
}

// emptySummary is the body for a caller scoped out of every event. With compare it carries the same window fields as a result that reached the store.
func emptySummary(q usagelog.EventQuery, compare bool) (usagelog.SummaryResult, error) {
	res := usagelog.SummaryResult{Rows: []usagelog.SummaryRow{}}
	if !compare {
		res.From, res.To = requestedWindow(q)
		return res, nil
	}
	from, to, err := usage.ResolveWindow(q, time.Now())
	if err != nil {
		return usagelog.SummaryResult{}, huma.Error400BadRequest(err.Error())
	}
	res.From, res.To = from, to
	res.PreviousFrom, res.PreviousTo = usage.PreviousWindow(from, to)
	return res, nil
}
