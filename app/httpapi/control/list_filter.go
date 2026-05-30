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
