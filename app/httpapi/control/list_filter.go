// list_filter.go wires the pure pkg/filter engine into the generic CRUD
// list handler. huma binds typed structs, but the filter allowlist is
// per-resource dynamic, so a small middleware stashes the raw url.Values on
// the request context and the list resolver reads them back to feed
// filterSchema.Parse. See registerKind in crud.go and the per-resource
// schemas in list_schemas.go.
package control

import (
	"context"
	"net/http"
	"net/url"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/filter"
)

type ctxKey int

const rawQueryKey ctxKey = iota

// withRawQuery returns protect with a leading middleware that stashes
// r.URL.Query() on the request context, so the (typed-input) list resolver
// can reach the raw params for schema-driven filtering.
func withRawQuery(protect huma.Middlewares) huma.Middlewares {
	mw := httpapi.HumaAuth(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), rawQueryKey, r.URL.Query())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	out := make(huma.Middlewares, 0, len(protect)+1)
	out = append(out, mw)
	out = append(out, protect...)
	return out
}

// rawQueryFrom returns the stashed query params, or an empty url.Values when
// none were stashed (no filterSchema route).
func rawQueryFrom(ctx context.Context) url.Values {
	if v, ok := ctx.Value(rawQueryKey).(url.Values); ok {
		return v
	}
	return url.Values{}
}

// filterParams converts a schema's accepted query params into huma operation
// parameters, so the list endpoint's filter surface appears in /openapi.json
// (and thus in clients generated from it). Derived from the same Field list
// that drives matching — no hand-maintained second list.
func filterParams[T any](s *filter.Schema[T]) []*huma.Param {
	descs := s.Params()
	out := make([]*huma.Param, 0, len(descs))
	for _, d := range descs {
		schema := &huma.Schema{Type: d.Type}
		if len(d.Enum) > 0 {
			schema.Enum = make([]any, len(d.Enum))
			for i, e := range d.Enum {
				schema.Enum[i] = e
			}
		}
		p := &huma.Param{Name: d.Name, In: "query", Description: d.Description, Schema: schema}
		if d.Repeatable {
			explode := true
			p.Explode = &explode
			p.Schema = &huma.Schema{Type: "array", Items: schema}
		}
		out = append(out, p)
	}
	return out
}
