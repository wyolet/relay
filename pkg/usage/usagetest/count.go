package usagetest

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/usage"
)

type countCase struct {
	name  string
	q     usage.EventQuery
	limit int
	want  int64
}

func windowWith(edit func(*usage.EventQuery)) usage.EventQuery {
	q := Window()
	edit(&q)
	return q
}

var countCases = []countCase{
	{
		name: "counts log-only events like the listing",
		q:    Window(),
		want: 12,
	},
	{
		name: "status filter",
		q:    windowWith(func(q *usage.EventQuery) { q.Status = []int{429} }),
		want: 2,
	},
	{
		name: "relay_key filter includes its log-only event",
		q:    windowWith(func(q *usage.EventQuery) { q.RelayKey = []string{"key-a"} }),
		want: 4,
	},
	{
		name: "a log-only error kind alone",
		q:    windowWith(func(q *usage.EventQuery) { q.ErrorKind = []string{"no_keys"} }),
		want: 1,
	},
	{
		name: "read scope is a disjunction of project and principal",
		q: windowWith(func(q *usage.EventQuery) {
			q.ScopeProjectID, q.ScopePrincipalID = []string{"project-a"}, []string{"principal-a"}
		}),
		want: 4,
	},
	{
		name: "read scope composes with a filter",
		q: windowWith(func(q *usage.EventQuery) {
			q.ScopeProjectID, q.ScopePrincipalID = []string{"project-a"}, []string{"principal-a"}
			q.Source = []string{"proxy"}
		}),
		want: 2,
	},
	{
		// r06 sits exactly on the bound and is excluded.
		name: "the exclusive before bound applies",
		q:    windowWith(func(q *usage.EventQuery) { q.Before = at(65) }),
		want: 6,
	},
	{
		name: "page limit and cursor are ignored",
		q: windowWith(func(q *usage.EventQuery) {
			q.Limit, q.CursorTS, q.CursorID = 3, at(60), "r06"
		}),
		want: 12,
	},
	{
		name:  "counting stops at the limit",
		q:     Window(),
		limit: 5,
		want:  5,
	},
	{
		name:  "a limit above the match count is exact",
		q:     Window(),
		limit: 13,
		want:  12,
	},
}

func runCount(t *testing.T, r usage.Reader) {
	for _, tc := range countCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.Count(t.Context(), tc.q, tc.limit)
			if err != nil {
				t.Fatalf("Count: %v", err)
			}
			if got != tc.want {
				t.Errorf("count = %d, want %d", got, tc.want)
			}
			if tc.limit > 0 {
				return
			}
			listed := tc.q
			listed.Limit, listed.CursorTS, listed.CursorID = usage.MaxEventLimit, time.Time{}, ""
			events, err := r.Events(t.Context(), listed)
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			if int64(len(events)) != got {
				t.Errorf("count = %d, but Events lists %d", got, len(events))
			}
		})
	}
}
