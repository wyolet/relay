// Package systemrl provides HTTP middleware that enforces system-level
// rate limits — rate limits stored in the catalog under well-known names
// (e.g. "system-api", "inference-proxy-anonymous") that apply at the transport
// edge rather than per-key in the pipeline.
//
// Two runtime buckets are wired at startup:
//   - "system-api"                  — per-IP, control plane + non-inference
//   - "inference-proxy-anonymous"   — per-IP, anonymous proxy inference requests
//
// Config-ceiling buckets ("inference", "inference-proxy") are catalog entries
// but are NOT enforced here — they are enforced at admin write time.
//
// On Reserve failure that isn't *ExceededError the middleware fails open (logs
// warn, passes the request through) so a KV outage doesn't cause a total outage.
package systemrl

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/wyolet/relay/internal/catalog"
	"github.com/wyolet/relay/internal/transport/mode"
	pkgrl "github.com/wyolet/relay/pkg/ratelimit"
)

// snapshotFn returns the current catalog snapshot via the Store interface;
// we only need RateLimitByName so we accept the full Store for simplicity.
type snapshotFn func() catalog.Store

// Middleware returns an HTTP middleware that enforces the named rate-limit
// bucket from the catalog snapshot.
//
// Parameters:
//   - limiter    — pkg/ratelimit.Limiter (shared, process-wide).
//   - storeFn    — returns the current catalog.Store; called per-request so
//     changes to the catalog (enable/disable, rule edits) take effect without
//     restart.
//   - bucket     — catalog RateLimit name to look up (e.g. "system-api").
//   - scopeFn    — builds the per-request scope string (e.g. "ip:10.0.0.1").
//
// When the bucket is not found in the catalog or its Spec.Enabled is
// explicitly false, the middleware is a transparent pass-through. When the
// bucket exists and is enabled, scopeFn is called and Reserve is attempted.
// A 429 with a clean JSON envelope + Retry-After header is written on
// ExceededError. All other Reserve errors log warn and pass through (fail-open).
//
// The scope string is prefixed "system:<bucket>:" so keys are distinct from
// per-policy pipeline scopes.
func Middleware(
	limiter *pkgrl.Limiter,
	storeFn snapshotFn,
	bucket string,
	scopeFn func(*http.Request) string,
) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			store := storeFn()
			rl, ok := store.RateLimitByName(bucket)
			if !ok || !catalog.IsEnabled(rl.Spec.Enabled) {
				next.ServeHTTP(w, r)
				return
			}

			scope := "system:" + bucket + ":" + scopeFn(r)
			rules := toRules(rl)
			if len(rules) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			_, err := limiter.Reserve(r.Context(), scope, rules)
			if err != nil {
				var ee *pkgrl.ExceededError
				if errors.As(err, &ee) {
					write429(w, ee)
					return
				}
				// Fail-open: non-exceeded error (KV outage, script error, etc.)
				slog.Warn("systemrl: reserve failed, failing open",
					"bucket", bucket,
					"err", err,
				)
				next.ServeHTTP(w, r)
				return
			}
			// Commit is best-effort async. For request-counter meters this is
			// sufficient — the Reserve already incremented the counter. Token
			// post-hoc increment is not applicable at this layer (no token counts).
			next.ServeHTTP(w, r)
		})
	}
}

// ConditionalAnonymousMiddleware wraps Middleware so it only fires when the
// request is classified as ModeProxyAnonymous. All other modes pass through
// without touching the limiter.
//
// This is the correct wiring for the "inference-proxy-anonymous" bucket: the
// per-key pipeline handles rate limiting for authed requests, so we only need
// to cap anonymous proxy traffic here.
func ConditionalAnonymousMiddleware(
	limiter *pkgrl.Limiter,
	storeFn snapshotFn,
	bucket string,
	scopeFn func(*http.Request) string,
) func(http.Handler) http.Handler {
	inner := Middleware(limiter, storeFn, bucket, scopeFn)
	return func(next http.Handler) http.Handler {
		innerH := inner(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cls, err := mode.Classify(r)
			if err != nil || cls.Mode != mode.ModeProxyAnonymous {
				next.ServeHTTP(w, r)
				return
			}
			innerH.ServeHTTP(w, r)
		})
	}
}

// toRules converts a catalog.RateLimit to []pkgrl.Rule. The scope prefix is
// handled externally; Key here is per-rule only (rl-name + meter).
func toRules(rl *catalog.RateLimit) []pkgrl.Rule {
	normalised := rl.Spec.NormalizedRules()
	out := make([]pkgrl.Rule, 0, len(normalised))
	for _, r := range normalised {
		strategy := pkgrl.Strategy(r.Strategy)
		if strategy == "" {
			strategy = pkgrl.StrategyTokenBucket
		}
		w := r.Window
		meter := r.Meter
		if meter == "" {
			meter = string(catalog.MeterRequests)
		}
		out = append(out, pkgrl.Rule{
			Key:      rl.Metadata.Name + ":" + meter,
			Name:     rl.Metadata.Name + " " + meter,
			Meter:    meter,
			Strategy: strategy,
			Amount:   r.Amount,
			Window:   w,
		})
	}
	return out
}

var deny429 = mustMarshal(map[string]any{
	"error": map[string]any{
		"type":    "rate_limit_exceeded",
		"code":    "rate_limit_exceeded",
		"message": "rate limit exceeded",
	},
})

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func write429(w http.ResponseWriter, ee *pkgrl.ExceededError) {
	w.Header().Set("Content-Type", "application/json")
	if ee.RetryAfter > 0 {
		secs := int(ee.RetryAfter / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = w.Write(deny429)
}
