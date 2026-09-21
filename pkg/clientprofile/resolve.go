package clientprofile

import (
	"context"
	"net/http"
	"strings"

	"github.com/wyolet/relay/pkg/httpheader"
)

// Resolve picks the profile for r: the X-WR-Client header wins, then the
// first path segment, then a User-Agent Match, then Default. An unknown
// header or prefix value falls through rather than erroring — a client
// naming a profile this deployment does not serve gets today's behaviour.
func (reg *Registry) Resolve(r *http.Request) Profile {
	if reg == nil || len(reg.order) == 0 {
		return Default
	}
	if v := r.Header.Get(httpheader.HeaderClient); v != "" {
		if p, ok := reg.byName[v]; ok {
			return p
		}
	}
	if seg := FirstPathSegment(r.URL.Path); seg != "" {
		if p, ok := reg.byName[seg]; ok {
			return p
		}
	}
	for _, p := range reg.order {
		if p.Match(r) {
			return p
		}
	}
	return Default
}

// FirstPathSegment returns the first non-empty path segment of p
// ("/claude-code/v1/messages" → "claude-code"), without allocating.
func FirstPathSegment(p string) string {
	p = strings.TrimPrefix(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}

type ctxKey struct{}

// WithProfile stores p on ctx.
func WithProfile(ctx context.Context, p Profile) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the profile stored on ctx, or Default when unset.
func FromContext(ctx context.Context) Profile {
	if p, ok := ctx.Value(ctxKey{}).(Profile); ok && p != nil {
		return p
	}
	return Default
}

// Middleware resolves the profile once per request and stores it on the
// context. A nil or empty registry is a pass-through, and so is a request
// that resolves to Default — neither allocates a context value.
func Middleware(reg *Registry) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if reg == nil || len(reg.order) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := reg.Resolve(r)
			if p.Name() == "" {
				next.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r.WithContext(WithProfile(r.Context(), p)))
		})
	}
}
