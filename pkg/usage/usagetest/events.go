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
		name: "refusal filter",
		q: func() usage.EventQuery {
			q := Window()
			q.Refusal = []string{"no_capacity", "model_not_allowed"}
			return q
		}(),
		want: []string{"r11"},
	},
	{
		name: "pool_id filter",
		q: func() usage.EventQuery {
			q := Window()
			q.PoolID = []string{"pool-a"}
			return q
		}(),
		want: []string{"r02", "r01"},
	},
	{
		name: "account_id filter",
		q: func() usage.EventQuery {
			q := Window()
			q.AccountID = []string{"acct-b"}
			return q
		}(),
		want: []string{"r03"},
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
	t.Run("pool and account ids read back", func(t *testing.T) {
		q := Window()
		q.RequestID = "r03"
		events, err := r.Events(t.Context(), q)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("Events: got %d events, want 1", len(events))
		}
		assertEqual(t, "pool and account", [2]string{events[0].PoolID, events[0].AccountID}, [2]string{"pool-b", "acct-b"})
	})
	t.Run("refusal reads back", func(t *testing.T) {
		q := Window()
		q.RequestID = "r00"
		events, err := r.Events(t.Context(), q)
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("Events: got %d events, want 1", len(events))
		}
		assertEqual(t, "refusal", events[0].Refusal, "host_not_reachable")
	})
}
