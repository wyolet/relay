package usagetest

import (
	"testing"

	"github.com/wyolet/relay/pkg/usage"
)

type facetsCase struct {
	name  string
	q     usage.EventQuery
	dims  []string
	limit int
	want  map[string][]usage.FacetValue
}

func fv(value, label string, requests int64) usage.FacetValue {
	return usage.FacetValue{Value: value, Label: label, Requests: requests}
}

var facetsCases = []facetsCase{
	{
		name: "every dimension counts the window without log-only rows",
		q:    Window(),
		dims: []string{"model_id", "host_id", "relay_key_hash", "source", "error_kind", "model", "tags.env"},
		want: map[string][]usage.FacetValue{
			"model_id": {fv("mid-a", "model-a", 6), fv("mid-b", "model-b", 4)},
			// hid-1's newest event has no slug and its log-only one is ignored, so the latest non-empty slug wins; "" sorts before hid-3 on the tie.
			"host_id":        {fv("hid-1", "host-new", 5), fv("hid-2", "host-b", 3), fv("", "", 1), fv("hid-3", "", 1)},
			"relay_key_hash": {fv("hash-b", "key-b", 6), fv("hash-a", "key-a", 3), fv("", "", 1)},
			"source":         {fv("pipeline", "", 5), fv("proxy", "", 5)},
			"error_kind":     {fv("", "", 10)},
			"model":          {fv("model-a", "", 6), fv("model-b", "", 4)},
			"tags.env":       {fv("", "", 8), fv("prod", "", 2)},
		},
	},
	{
		name: "a dimension ignores its own filter and keeps the others",
		q: func() usage.EventQuery {
			q := Window()
			q.ModelID, q.HostID = []string{"mid-b"}, []string{"hid-2"}
			return q
		}(),
		dims: []string{"model_id", "host_id"},
		want: map[string][]usage.FacetValue{
			"model_id": {fv("mid-b", "model-b", 2), fv("mid-a", "model-a", 1)},
			"host_id":  {fv("hid-2", "host-b", 2), fv("", "", 1), fv("hid-1", "host-new", 1)},
		},
	},
	{
		name: "a tag dimension ignores its own tag filter",
		q: func() usage.EventQuery {
			q := Window()
			q.Tags = map[string][]string{"env": {"prod"}}
			return q
		}(),
		dims: []string{"tags.env", "model_id"},
		want: map[string][]usage.FacetValue{
			"tags.env": {fv("", "", 8), fv("prod", "", 2)},
			"model_id": {fv("mid-a", "model-a", 2)},
		},
	},
	{
		name: "a slug dimension ignores its own filter",
		q: func() usage.EventQuery {
			q := Window()
			q.RelayKey = []string{"key-a"}
			return q
		}(),
		dims: []string{"relay_key", "relay_key_hash"},
		want: map[string][]usage.FacetValue{
			"relay_key":      {fv("key-b", "", 6), fv("key-a", "", 3), fv("", "", 1)},
			"relay_key_hash": {fv("hash-a", "key-a", 3)},
		},
	},
	{
		name: "the label is the latest slug inside the window",
		q: func() usage.EventQuery {
			q := Window()
			q.To = at(30)
			return q
		}(),
		dims: []string{"host_id"},
		want: map[string][]usage.FacetValue{
			"host_id": {fv("hid-1", "host-old", 2), fv("hid-2", "host-b", 2)},
		},
	},
	{
		name: "an exclusive end leaves out the event on it",
		q: func() usage.EventQuery {
			q := Window()
			q.Before = at(30)
			return q
		}(),
		dims: []string{"host_id"},
		want: map[string][]usage.FacetValue{
			"host_id": {fv("hid-1", "host-old", 2), fv("hid-2", "host-b", 1)},
		},
	},
	{
		name:  "limit keeps the most frequent values",
		q:     Window(),
		dims:  []string{"host_id"},
		limit: 2,
		want: map[string][]usage.FacetValue{
			"host_id": {fv("hid-1", "host-new", 5), fv("hid-2", "host-b", 3)},
		},
	},
}

func runFacets(t *testing.T, r usage.Reader) {
	for _, tc := range facetsCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := usage.Facets(t.Context(), r, tc.q, tc.dims, tc.limit)
			if err != nil {
				t.Fatalf("Facets: %v", err)
			}
			assertEqual(t, "facets", got, tc.want)
		})
	}
	t.Run("none is not a dimension", func(t *testing.T) {
		if _, err := r.Facet(t.Context(), usage.FacetQuery{EventQuery: Window(), Dim: usage.GroupByNone}); err == nil {
			t.Fatal("Facet accepted dim none")
		}
	})
}
