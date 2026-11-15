package catalogprune

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

var (
	windowStart = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	lastSeen    = time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
)

func cand(kind, name string, refs ...Ref) *candidate {
	return &candidate{kind: kind, id: kind + "/" + name, name: name, refs: refs}
}

func ref(kind, name string) Ref { return Ref{Kind: kind, ID: kind + "/" + name, Name: name} }

func dirty(c *candidate) *candidate    { c.dirty = true; return c }
func overlaid(c *candidate) *candidate { c.overlay = true; return c }
func withUsage(c *candidate, last time.Time, unknown string) *candidate {
	c.usage = &usageFact{since: windowStart, last: last, unknown: unknown}
	return c
}

type outcome struct{ Decision, Reason string }

func TestDecide(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cands []*candidate
		want  map[string]outcome
	}{
		{"clean, unreferenced, idle", []*candidate{withUsage(cand("model", "m"), time.Time{}, "")},
			map[string]outcome{"m": {Prune, ""}}},
		{"edited", []*candidate{dirty(cand("policy", "tier-0"))},
			map[string]outcome{"tier-0": {Keep, "edited"}}},
		{"overlay", []*candidate{overlaid(cand("model", "m"))},
			map[string]outcome{"m": {Keep, "overlay"}}},
		{"referenced by a row no prune removes", []*candidate{cand("rate-limit", "rl", ref("policy", "team-default"))},
			map[string]outcome{"rl": {Keep, "referenced by policy team-default"}}},
		{"names three referencing rows and counts the rest", []*candidate{cand("host", "h",
			ref("host-key", "k1"), ref("account", "a1"), ref("pool", "p1"), ref("host-key", "k2"))},
			map[string]outcome{"h": {Keep, "referenced by account a1, host-key k1, host-key k2 and 1 more"}}},
		{"traffic inside the window", []*candidate{withUsage(cand("host-binding", "b"), lastSeen, "")},
			map[string]outcome{"b": {Keep, "traffic since 2026-10-16T00:00:00Z (last request 2026-11-10T09:30:00Z)"}}},
		{"usage unknown", []*candidate{withUsage(cand("model", "m"), time.Time{}, "no usage store is configured")},
			map[string]outcome{"m": {Keep, "usage unknown: no usage store is configured"}}},
		{"children pruned with their parent", []*candidate{
			withUsage(cand("host-binding", "b"), time.Time{}, ""),
			cand("pricing", "p", ref("host-binding", "b")),
			withUsage(cand("model", "m", ref("host-binding", "b"), ref("pricing", "p")), time.Time{}, ""),
			cand("provider", "acme", ref("model", "m")),
		}, map[string]outcome{"b": {Prune, ""}, "p": {Prune, ""}, "m": {Prune, ""}, "acme": {Prune, ""}}},
		{"an edited child keeps its parents", []*candidate{
			dirty(cand("host-binding", "b")),
			cand("pricing", "p", ref("host-binding", "b")),
			withUsage(cand("model", "m", ref("host-binding", "b"), ref("pricing", "p")), time.Time{}, ""),
			cand("provider", "acme", ref("model", "m")),
			cand("provider", "other"),
		}, map[string]outcome{
			"b":     {Keep, "edited"},
			"p":     {Keep, "referenced by host-binding b (kept)"},
			"m":     {Keep, "referenced by host-binding b (kept), pricing p (kept)"},
			"acme":  {Keep, "referenced by model m (kept)"},
			"other": {Prune, ""},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]outcome{}
			for _, d := range decide(tc.cands) {
				got[d.Name] = outcome{d.Decision, d.Reason}
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatalf("decisions (-want +got):\n%s", diff)
			}
		})
	}
}

// Rows come back in delete order, then by name.
func TestDecideOrdersRowsTheWayDeletesRun(t *testing.T) {
	ds := decide([]*candidate{cand("provider", "a"), cand("model", "z"), cand("model", "b"), cand("host-binding", "y")})
	var got []string
	for _, d := range ds {
		got = append(got, d.Kind+"/"+d.Name)
	}
	if diff := cmp.Diff([]string{"host-binding/y", "model/b", "model/z", "provider/a"}, got); diff != "" {
		t.Fatalf("order (-want +got):\n%s", diff)
	}
}
