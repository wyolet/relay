package reqid

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"sync"

	"github.com/oklog/ulid/v2"
	"go.opentelemetry.io/otel"

	"github.com/wyolet/relay/pkg/usage"
)

const (
	HeaderInbound  = "X-Request-ID"
	HeaderOutbound = "X-Relay-Request-ID"
)

type ctxKey int

const (
	ctxKeyID ctxKey = iota
	ctxKeyLogger
	ctxKeyAttribution
)

var (
	entropyMu sync.Mutex
	entropy   = ulid.Monotonic(rand.Reader, 0)
)

func Generate() string {
	entropyMu.Lock()
	id := ulid.MustNew(ulid.Now(), entropy)
	entropyMu.Unlock()
	return id.String()
}

func isValidInbound(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

func Middleware(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(HeaderInbound)
			if !isValidInbound(id) {
				id = Generate()
			}
			w.Header().Set(HeaderOutbound, id)
			logger := base.With(slog.String("request_id", id))
			ctx := r.Context()
			ctx = context.WithValue(ctx, ctxKeyID, id)
			ctx = context.WithValue(ctx, ctxKeyLogger, logger)
			attr := usage.ParseMetadataHeader(r.Header.Get("X-Relay-Metadata"))
			ctx = context.WithValue(ctx, ctxKeyAttribution, attr)
			ctx, sp := otel.Tracer("relay").Start(ctx, usage.SpanName)
			ctx = usage.ContextWithSpan(ctx, sp)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Attribution returns the parsed X-Relay-Metadata map from ctx, or nil.
func Attribution(ctx context.Context) map[string]string {
	v, _ := ctx.Value(ctxKeyAttribution).(map[string]string)
	return v
}

func From(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyID).(string)
	return v
}

func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
