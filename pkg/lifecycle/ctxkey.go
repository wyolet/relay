package lifecycle

import "context"

// ctxKey is the private context-value key carrying the per-request
// Context. One Context is minted at request entry, stashed here, and
// read by every downstream phase (routing, runner, observers) so they
// all enrich and observe the same object rather than minting their own.
type ctxKey struct{}

// ContextWith returns a child ctx carrying c. Called once at request
// entry, after the request id and classification are known.
func ContextWith(ctx context.Context, c *Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// FromContext returns the per-request Context stashed by ContextWith, or
// nil if none was minted (e.g. a non-inference code path). Callers must
// nil-check; the lifecycle marks + accessors are all nil-safe.
func FromContext(ctx context.Context) *Context {
	c, _ := ctx.Value(ctxKey{}).(*Context)
	return c
}
