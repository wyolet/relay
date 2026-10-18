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

// compareWindow is what a summary compares its rows with: nothing, the preceding window of equal length, or the explicit [from, to).
type compareWindow struct {
	preceding bool
	from, to  time.Time
}

func (c compareWindow) explicit() bool { return !c.from.IsZero() }

func parseCompare(compare, rawFrom, rawTo string) (compareWindow, error) {
	from, err := parseTime("compare_from", rawFrom)
	if err != nil {
		return compareWindow{}, err
	}
	to, err := parseTime("compare_to", rawTo)
	if err != nil {
		return compareWindow{}, err
	}
	switch compare {
	case "":
	case comparePrevious:
		if !from.IsZero() || !to.IsZero() {
			return compareWindow{}, huma.Error400BadRequest("`compare` and `compare_from`/`compare_to` are alternatives; give one")
		}
		return compareWindow{preceding: true}, nil
	default:
		return compareWindow{}, huma.Error400BadRequest("`compare` must be \"previous\"")
	}
	if from.IsZero() != to.IsZero() {
		return compareWindow{}, huma.Error400BadRequest("`compare_from` and `compare_to` must be given together")
	}
	if !from.IsZero() && !to.After(from) {
		return compareWindow{}, huma.Error400BadRequest("`compare_to` must be after `compare_from`")
	}
	return compareWindow{from: from, to: to}, nil
}

// readSummary runs q, attaching each row's numbers over the compare window when one was asked for.
func readSummary(ctx context.Context, r usagelog.Reader, q usagelog.SummaryQuery, cw compareWindow) (usagelog.SummaryResult, error) {
	if cw.explicit() {
		return usage.SummaryWithPrevious(ctx, r, q, cw.from, cw.to)
	}
	if !cw.preceding {
		return r.Summary(ctx, q)
	}
	res, err := usage.SummaryWithPrecedingWindow(ctx, r, q)
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

// emptySummary is the body for a caller scoped out of every event. With a comparison it carries the same window fields as a result that reached the store.
func emptySummary(q usagelog.EventQuery, cw compareWindow) (usagelog.SummaryResult, error) {
	res := usagelog.SummaryResult{Rows: []usagelog.SummaryRow{}}
	if !cw.preceding {
		res.From, res.To = requestedWindow(q)
		res.PreviousFrom, res.PreviousTo = cw.from, cw.to
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
