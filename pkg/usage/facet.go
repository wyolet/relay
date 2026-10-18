package usage

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// FacetQuery asks for the most frequent values of one dimension over the rows matching EventQuery. Dim must be a group dimension other than GroupByNone.
type FacetQuery struct {
	EventQuery
	Dim   string
	Limit int
}

// FacetValue is one value of a facet dimension and the number of requests that carry it. Label is the latest non-empty event-time slug for an id dimension (see LabelColumn), empty otherwise.
type FacetValue struct {
	Value    string `json:"value"`
	Label    string `json:"label,omitempty"`
	Requests int64  `json:"requests"`
}

// FacetsResult maps each requested dimension to its top values, with the window the query covered.
type FacetsResult struct {
	Facets map[string][]FacetValue `json:"facets"`
	From   time.Time               `json:"from"`
	To     time.Time               `json:"to"`
}

// Facet request bounds: values per dimension (default and cap) and dimensions per request.
const (
	DefaultFacetLimit = 50
	MaxFacetLimit     = 200
	MaxFacetDims      = 16
)

// facetParallelism bounds the per-dimension queries in flight; the SQL backends pool four connections.
const facetParallelism = 4

// FacetLimit clamps a requested facet limit to [1, MaxFacetLimit], defaulting to DefaultFacetLimit.
func FacetLimit(limit int) int {
	if limit <= 0 {
		return DefaultFacetLimit
	}
	return min(limit, MaxFacetLimit)
}

// IsValidFacetDim reports whether dim can be faceted: any group dimension except GroupByNone.
func IsValidFacetDim(dim string) bool {
	return Grouped(dim) && IsValidGroupBy(dim)
}

// Facets runs one Facet query per dimension, each with q minus that dimension's own filter (WithoutFilter), so a dropdown lists the values the other filters still allow.
func Facets(ctx context.Context, r Reader, q EventQuery, dims []string, limit int) (map[string][]FacetValue, error) {
	out := make(map[string][]FacetValue, len(dims))
	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(facetParallelism)
	for _, dim := range dims {
		g.Go(func() error {
			vals, err := r.Facet(gctx, FacetQuery{EventQuery: WithoutFilter(q, dim), Dim: dim, Limit: limit})
			if err != nil {
				return fmt.Errorf("facet %s: %w", dim, err)
			}
			if vals == nil {
				vals = []FacetValue{}
			}
			mu.Lock()
			out[dim] = vals
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// TopValues counts the (already-filtered) events by dim and returns the most frequent values, requests desc then value asc. LogOnly events are skipped, as in Summarize.
func TopValues(events []Event, dim string, limit int) ([]FacetValue, error) {
	if !IsValidFacetDim(dim) {
		return nil, fmt.Errorf("usage.TopValues: invalid dim %q", dim)
	}
	var labels LatestLabels
	counts := map[string]int64{}
	for _, ev := range events {
		if ev.LogOnly() {
			continue
		}
		v := groupKey(ev, dim)
		counts[v]++
		labels.addEvent(dim, v, ev)
	}
	out := make([]FacetValue, 0, len(counts))
	for v, n := range counts {
		out = append(out, FacetValue{Value: v, Label: labels.Get(v), Requests: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Value < out[j].Value
	})
	if n := FacetLimit(limit); len(out) > n {
		out = out[:n]
	}
	return out, nil
}
