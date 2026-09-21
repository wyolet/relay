package adapter

import (
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"time"

	"github.com/wyolet/relay/pkg/metrics"
)

const (
	// defaultTimeout bounds two things: a non-streamed upstream call end to end, and — on every call — the wait for the upstream's response headers. It is deliberately NOT a whole-request cap on streams, where a legitimate generation runs for hours and only the pre-first-byte leg needs a deadline for failover.
	defaultTimeout = 5 * time.Minute

	// defaultStreamIdleTimeout ends a streamed upstream that has stopped sending bytes. Set well above the ~300 s silence watchdogs coding agents use, so a wedged stream is ended by the client's own timeout rather than by relay giving up under it.
	defaultStreamIdleTimeout = 10 * time.Minute

	// defaultMaxIdleConnsPerHost keeps hot upstream connections warm. The
	// stdlib default (2) re-dials nearly every request at high per-host RPS.
	// Overridable per deployment via SetUpstreamMaxIdleConnsPerHost.
	defaultMaxIdleConnsPerHost = 128

	// maxIdleConnsScale gives the total idle pool headroom over the per-host
	// cap so several hot hosts can each keep a full keep-alive pool.
	maxIdleConnsScale = 8

	idleConnTimeout       = 90 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	expectContinueTimeout = 1 * time.Second
)

// maxIdleConnsPerHost is the per-host idle-connection ceiling applied to
// every upstream transport built after it is set. Read at Build time.
var maxIdleConnsPerHost = defaultMaxIdleConnsPerHost

// streamIdleTimeout is the silence budget applied to every streamed upstream body read by specs built after it is set. Read at Build time.
var streamIdleTimeout = defaultStreamIdleTimeout

// SetUpstreamStreamIdleTimeout overrides the streamed-response idle deadline for every Spec built afterwards (the composition root wires RELAY_STREAM_IDLE_TIMEOUT_S here). Zero disables the deadline; negative values are ignored. Not safe to call concurrently with Build — invoke once at boot, before specs are constructed.
func SetUpstreamStreamIdleTimeout(d time.Duration) {
	if d >= 0 {
		streamIdleTimeout = d
	}
}

// SetUpstreamMaxIdleConnsPerHost overrides the per-host idle-connection cap
// used by every Spec built afterwards (the composition root wires the
// RELAY_UPSTREAM_MAX_IDLE_PER_HOST value here). Values < 1 are ignored so a
// zero config default leaves the built-in 128. Not safe to call concurrently
// with Build — invoke once at boot, before specs are constructed.
func SetUpstreamMaxIdleConnsPerHost(n int) {
	if n >= 1 {
		maxIdleConnsPerHost = n
	}
}

// NewUpstreamTransport builds the tuned upstream transport. http1 empties
// TLSNextProto on the same tuned base to disable HTTP/2 negotiation for
// shapes that trip Go's HTTP/2 client bugs (see Spec.UseHTTP1). Exported for
// the composition root, which applies the same pooling to the proxy runner's
// client (its upstreams are just as hot as the pipeline's).
//
// The returned RoundTripper wraps the transport with connection-reuse
// accounting (relay_upstream_connections_total) — the tripwire for the
// MaxIdleConnsPerHost-style churn this pooling exists to prevent.
func NewUpstreamTransport(http1 bool) http.RoundTripper {
	perHost := maxIdleConnsPerHost
	tr := &http.Transport{
		MaxIdleConns:        perHost * maxIdleConnsScale,
		MaxIdleConnsPerHost: perHost,
		IdleConnTimeout:     idleConnTimeout,
		// The only whole-response deadline a stream can tolerate: the wait for the upstream's headers. Past that the caller is reading real bytes and any cap would cut a live generation short.
		ResponseHeaderTimeout: defaultTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
	}
	if http1 {
		tr.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	}
	return connTrackingTransport{base: tr}
}

// connTrackingTransport counts whether each upstream attempt got a fresh
// dial or a pooled connection. httptrace composes with any trace already
// on the context, so this is transparent to callers.
type connTrackingTransport struct {
	base http.RoundTripper
}

var connTrace = &httptrace.ClientTrace{
	GotConn: func(info httptrace.GotConnInfo) { metrics.UpstreamConn(info.Reused) },
}

func (t connTrackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(
		req.WithContext(httptrace.WithClientTrace(req.Context(), connTrace)))
}
