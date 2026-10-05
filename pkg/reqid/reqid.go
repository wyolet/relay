// Package reqid gives each HTTP request a relay-minted ULID, echoes it as X-Relay-Request-ID, and carries it with a request-scoped logger in the context. A caller's X-Request-ID is carried separately as the client id.
package reqid

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"sync"

	"github.com/oklog/ulid/v2"
)

const (
	HeaderInbound  = "X-Request-ID"
	HeaderOutbound = "X-Relay-Request-ID"
)

type ctxKey int

const (
	ctxKeyID ctxKey = iota
	ctxKeyLogger
	ctxKeyClientID
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

// Middleware always mints the request id: it keys stored records, so a
// caller must not be able to choose or repeat it. A valid inbound
// X-Request-ID is kept apart as the client id and echoed back unchanged.
func Middleware(base *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := Generate()
			w.Header().Set(HeaderOutbound, id)
			logger := base.With(slog.String("request_id", id))
			ctx := r.Context()
			if cid := r.Header.Get(HeaderInbound); isValidInbound(cid) {
				w.Header().Set(HeaderInbound, cid)
				logger = logger.With(slog.String("client_request_id", cid))
				ctx = context.WithValue(ctx, ctxKeyClientID, cid)
			}
			ctx = context.WithValue(ctx, ctxKeyID, id)
			ctx = context.WithValue(ctx, ctxKeyLogger, logger)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// WithNewID derives a context carrying a freshly minted id, for requests
// multiplexed over one HTTP request (one WebSocket connection).
func WithNewID(ctx context.Context, base *slog.Logger) context.Context {
	id := Generate()
	ctx = context.WithValue(ctx, ctxKeyID, id)
	return context.WithValue(ctx, ctxKeyLogger, base.With(slog.String("request_id", id)))
}

func From(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyID).(string)
	return v
}

// ClientID returns the caller's own X-Request-ID, empty when absent or invalid.
func ClientID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyClientID).(string)
	return v
}

func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}
