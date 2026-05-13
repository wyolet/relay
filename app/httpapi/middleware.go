package httpapi

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
)

// HumaAuth adapts a net/http middleware into a huma per-operation middleware.
// Used by both planes to attach auth (or any http.Handler-shaped middleware)
// to specific huma operations rather than the whole chi router.
//
// Pattern: pass any `func(http.Handler) http.Handler` and it composes onto a
// huma.Operation via Middlewares = huma.Middlewares{HumaAuth(mw), ...}.
func HumaAuth(mw func(http.Handler) http.Handler) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		r, w := humachi.Unwrap(ctx)
		mw(http.HandlerFunc(func(w2 http.ResponseWriter, r2 *http.Request) {
			next(humachi.NewContext(ctx.Operation(), r2, w2))
		})).ServeHTTP(w, r)
	}
}
