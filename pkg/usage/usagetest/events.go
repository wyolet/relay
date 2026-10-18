package usagetest

import (
	"testing"

	"github.com/wyolet/relay/pkg/usage"
)

type eventsCase struct {
	name string
	q    usage.EventQuery
	want []string // request ids, newest first
}

var eventsCases = []eventsCase{
	{
		name: "listing keeps log-only events, newest first",
		q:    Window(),
		want: []string{"r11", "r10", "r09", "r08", "r07", "r06", "r05", "r04", "r03", "r02", "r01", "r00"},
	},
	{
		name: "limit keeps the newest",
		q: func() usage.EventQuery {
			q := Window()
			q.Limit = 3
			return q
		}(),
		want: []string{"r11", "r10", "r09"},
	},
	{
		name: "status filter",
		q: func() usage.EventQuery {
			q := Window()
			q.Status = []int{429}
			return q
		}(),
		want: []string{"r08", "r03"},
	},
	{
		name: "relay_key filter",
		q: func() usage.EventQuery {
			q := Window()
			q.RelayKey = []string{"key-a"}
			return q
		}(),
		want: []string{"r03", "r02", "r01", "r00"},
	},
	{
		name: "before is an exclusive upper bound",
		q: func() usage.EventQuery {
			q := Window()
			q.Before = at(45)
			return q
		}(),
		want: []string{"r04", "r03", "r02", "r01", "r00"},
	},
}

func runEvents(t *testing.T, r usage.Reader) {
	for _, tc := range eventsCases {
		t.Run(tc.name, func(t *testing.T) {
			events, err := r.Events(t.Context(), tc.q)
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			got := make([]string, len(events))
			for i, ev := range events {
				got[i] = ev.RequestID
			}
			assertEqual(t, "events", got, tc.want)
		})
	}
}
