package catalogprune

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/usage"
)

// Decision values.
const (
	Prune = "prune"
	Keep  = "keep"
)

// Ref is a row that references a candidate.
type Ref struct {
	// Kind is the referencing row's kind as an API singular (policy, host-key, …).
	Kind string
	ID   string
	Name string
}

// Decision is what a prune decided for one detached row.
type Decision struct {
	Kind       string    `json:"kind"       doc:"Kind of the row, as an API singular (model, host-binding, pricing, policy, rate-limit, host, provider)."`
	Name       string    `json:"name"       doc:"Slug of the row."`
	ID         string    `json:"id"         doc:"Id of the row."`
	DetachedAt time.Time `json:"detachedAt" doc:"When a catalog release stopped shipping the row."`
	Decision   string    `json:"decision"   enum:"prune,keep" doc:"prune: the row is deleted (or would be, in a dry run). keep: it stays detached and is judged again on the next prune."`
	Reason     string    `json:"reason,omitempty" doc:"Why the row stays: edited, overlay, referenced by <kind> <name> (kept when that row stays detached too), traffic since <time>, or usage unknown: <why>."`

	// Owner and version are what the delete and its audit row need; Deleted
	// is set once the row is gone, which a run that fails part-way needs said.
	Owner   meta.Owner `json:"-"`
	Deleted bool       `json:"-"`
	version string
}

// candidate is one detached row and the facts its decision rests on.
type candidate struct {
	kind, id, name string
	owner          meta.Owner
	detachedAt     time.Time
	version        string
	dirty          bool
	overlay        bool
	// traffic is the usage query of a kind with a traffic guard (models and
	// host bindings), and usage its answer.
	traffic *usage.EventQuery
	usage   *usageFact
	// refs are gathered only for a row that no fact above already keeps.
	refs []Ref
}

// usageFact is what the usage store said about a row's traffic in the idle
// window.
type usageFact struct {
	since time.Time // start of the idle window
	last  time.Time // newest request inside it; zero for none
	// unknown says why the store gave no answer; empty when it did.
	unknown string
}

// decide judges every candidate. A row referenced by a row that is not itself
// pruned stays; the pass repeats until no decision changes, so a child kept
// for a reason of its own keeps its parent too.
func decide(cands []*candidate) []Decision {
	byID := make(map[string]*candidate, len(cands))
	for _, c := range cands {
		byID[c.id] = c
	}
	reasons := make(map[string]string, len(cands))
	for _, c := range cands {
		if r := ownReason(c, byID); r != "" {
			reasons[c.id] = r
		}
	}
	for changed := true; changed; {
		changed = false
		for _, c := range cands {
			if _, kept := reasons[c.id]; kept {
				continue
			}
			var held []Ref
			for _, r := range c.refs {
				if _, kept := reasons[r.ID]; kept {
					held = append(held, r)
				}
			}
			if len(held) > 0 {
				reasons[c.id] = referencedBy(held, " (kept)")
				changed = true
			}
		}
	}
	out := make([]Decision, 0, len(cands))
	for _, c := range cands {
		d := Decision{Kind: c.kind, Name: c.name, ID: c.id, DetachedAt: c.detachedAt, Decision: Prune, Owner: c.owner, version: c.version}
		if r, kept := reasons[c.id]; kept {
			d.Decision, d.Reason = Keep, r
		}
		out = append(out, d)
	}
	sortDecisions(out)
	return out
}

// ownReason is why a row stays whatever the other candidates decide: an
// operator edit, an overlay, traffic or an unanswered usage check, or a
// reference from a row no prune would remove.
func ownReason(c *candidate, cands map[string]*candidate) string {
	switch {
	case c.dirty:
		return "edited"
	case c.overlay:
		return "overlay"
	}
	if u := c.usage; u != nil {
		switch {
		case !u.last.IsZero():
			return fmt.Sprintf("traffic since %s (last request %s)", u.since.UTC().Format(time.RFC3339), u.last.UTC().Format(time.RFC3339))
		case u.unknown != "":
			return "usage unknown: " + u.unknown
		}
	}
	var outside []Ref
	for _, r := range c.refs {
		if cands[r.ID] == nil {
			outside = append(outside, r)
		}
	}
	if len(outside) > 0 {
		return referencedBy(outside, "")
	}
	return ""
}

// maxNamedRefs caps the rows a reason names; a host can hold hundreds.
const maxNamedRefs = 3

// referencedBy names the referencing rows, each followed by suffix.
func referencedBy(refs []Ref, suffix string) string {
	sorted := append([]Ref(nil), refs...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Kind != sorted[j].Kind {
			return sorted[i].Kind < sorted[j].Kind
		}
		return sorted[i].Name < sorted[j].Name
	})
	names := make([]string, 0, maxNamedRefs)
	for _, r := range sorted[:min(len(sorted), maxNamedRefs)] {
		names = append(names, r.Kind+" "+r.Name+suffix)
	}
	s := "referenced by " + strings.Join(names, ", ")
	if more := len(sorted) - maxNamedRefs; more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}

// sortDecisions orders rows the way deletes run, then by name.
func sortDecisions(ds []Decision) {
	rank := make(map[string]int, len(deleteOrder))
	for i, k := range deleteOrder {
		rank[k] = i
	}
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].Kind != ds[j].Kind {
			return rank[ds[i].Kind] < rank[ds[j].Kind]
		}
		return ds[i].Name < ds[j].Name
	})
}
