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
	HeaderInbound  = "X-Request-Id"
	HeaderOutbound = "X-Relay-Request-Id"
)

type ctxKey struct{}

// state is the one context value a request's ids ride in. The logger is built only when something asks for it: most requests never log.
type state struct {
	id       string
	clientID string
	// logClientID is false for a state WithNewID derived: it inherits the caller's client id but does not log it.
	logClientID bool
	base        *slog.Logger
	once        sync.Once
	logger      *slog.Logger
}

func (s *state) log() *slog.Logger {
	s.once.Do(func() {
		s.logger = s.base.With(slog.String("request_id", s.id))
		if s.logClientID {
			s.logger = s.logger.With(slog.String("client_request_id", s.clientID))
		}
	})
	return s.logger
}

func stateFrom(ctx context.Context) *state {
	s, _ := ctx.Value(ctxKey{}).(*state)
	return s
}

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
			s := &state{id: Generate(), base: base}
			w.Header().Set(HeaderOutbound, s.id)
			if cid := r.Header.Get(HeaderInbound); isValidInbound(cid) {
				w.Header().Set(HeaderInbound, cid)
				s.clientID, s.logClientID = cid, true
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s)))
		})
	}
}

// WithNewID derives a context carrying a freshly minted id, for requests
// multiplexed over one HTTP request (one WebSocket connection).
func WithNewID(ctx context.Context, base *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, &state{id: Generate(), clientID: ClientID(ctx), base: base})
}

func From(ctx context.Context) string {
	if s := stateFrom(ctx); s != nil {
		return s.id
	}
	return ""
}

// ClientID returns the caller's own X-Request-ID, empty when absent or invalid.
func ClientID(ctx context.Context) string {
	if s := stateFrom(ctx); s != nil {
		return s.clientID
	}
	return ""
}

func Logger(ctx context.Context) *slog.Logger {
	if s := stateFrom(ctx); s != nil {
		return s.log()
	}
	return slog.Default()
}
