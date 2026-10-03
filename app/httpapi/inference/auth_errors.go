package inference

import (
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/wyolet/relay/pkg/metrics"
)

// authRejectedTotal answers "are callers being turned away at the edge, and
// why" without one log line per rejection: an unauthenticated flood would
// otherwise amplify into the log pipeline.
var authRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: metrics.Namespace,
	Subsystem: "inference",
	Name:      "auth_rejected_total",
	Help:      "Inference requests refused by the credential middleware, by reason.",
}, []string{"reason"})

func init() { metrics.Register(authRejectedTotal) }

// writeForbidden reports a caller that authenticated but may not proceed.
func writeForbidden(w http.ResponseWriter, code, msg string) {
	authRejectedTotal.WithLabelValues(code).Inc()
	slog.Debug("inference: auth rejected", "status", 403, "code", code, "msg", msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"` + code + `","message":"` + msg + `"}}`))
}

// writeAuthErr rejects an unauthenticated caller. msg doubles as the metric
// reason: every call site passes one of a fixed set of literals.
func writeAuthErr(w http.ResponseWriter, msg string) {
	// Revocation answers one code whichever path caught it — the version
	// bump here, the jti denylist inside the reservation.
	code := "unauthenticated"
	if msg == msgTokenRevoked {
		code = "token_revoked"
	}
	authRejectedTotal.WithLabelValues(msg).Inc()
	slog.Debug("inference: auth rejected", "status", 401, "code", code, "msg", msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"` + code + `","message":"` + msg + `"}}`))
}

// msgTokenRevoked is the message the token-version check answers with; the
// WebSocket recheck reuses it, so both report the same code.
const msgTokenRevoked = "token revoked"
