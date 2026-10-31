// Package proxy is the second inference flow: transparent forwarding
// of caller-supplied upstream credentials. Dispatched from the
// inference handler when X-WR-Proxy-Mode: Proxy is set.
//
// Differences from app/pipeline:
//   - No Policy / Model / HostBinding resolution. The caller picks the
//     upstream Host by slug via X-WR-Upstream-Host; the inference
//     handler hands us the resolved BaseURL.
//   - No keypool, no per-key circuit breaker, no retry/failover. The
//     caller's Authorization is the upstream credential; one attempt.
//   - An authed request holds its caller ladder's counters as normal
//     mode does, with the inference-api-proxy system bucket as a further
//     ceiling: bringing an upstream key must not lift the operator's
//     limits. Anonymous requests hold only inference-api-proxy-anonymous,
//     keyed by client IP.
//   - Same tee + detached post-flight pattern as app/pipeline: the
//     post-flight goroutine commits the reservation and extracts
//     tokens for usage logging. Never blocks the response.
//
// The package is HTTP-aware only at the upstream-call boundary (one
// outbound http.Request). The inbound *http.Request is consumed by the
// handler; this package takes pre-resolved bytes + headers + the upstream
// Authorization value.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/pkg/contentcoding"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	sdkusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// TokenExtractor extracts upstream usage from a response body. The
// inference handler picks the right extractor by inbound endpoint
// (OpenAI shape for /v1/chat/completions, Anthropic shape for
// /v1/messages) and passes it in.
type TokenExtractor interface {
	ExtractTokens(body []byte) sdkusage.Tokens
}

// ResponseSummarizer is the optional capability of a TokenExtractor that reads a response's canonical Summary, which then serves both the commit and the usage hook. false means it has no translator for the body.
type ResponseSummarizer interface {
	SummarizeResponse(body []byte) (v1.Summary, bool)
}

// chargedTokens returns the tokens the commit charges: the Summary's, decoded once and shared with the usage hook. A Summary without tokens (a stream that ended on an error event or before its terminal frame) can sit beside usage the upstream did report; those tokens were served, so ExtractTokens is asked for them.
func chargedTokens(lc *lifecycle.Context, e TokenExtractor, body []byte) sdkusage.Tokens {
	var tokens sdkusage.Tokens
	if sum, ok := responseSummary(lc, e, body); ok {
		tokens = sum.Tokens
	}
	if len(tokens) == 0 && e != nil {
		tokens = e.ExtractTokens(body)
	}
	return tokens
}

// responseSummary returns the Summary e reads from body, recorded on lc so the usage hook does not decode the body again.
func responseSummary(lc *lifecycle.Context, e TokenExtractor, body []byte) (v1.Summary, bool) {
	rs, ok := e.(ResponseSummarizer)
	if !ok {
		return v1.Summary{}, false
	}
	s, ok := rs.SummarizeResponse(body)
	if ok {
		lc.SetResponseSummary(s)
	}
	return s, ok
}

// Request is the pre-resolved input to Run.
type Request struct {
	// Method + Path are the inbound HTTP method and path appended to
	// HostBaseURL when building the upstream URL. Path includes the
	// leading slash (e.g. "/v1/messages").
	Method string
	Path   string

	// Body is the inbound request body. Streamed through to upstream
	// verbatim; the proxy does not buffer it. Transport-layer code that
	// wants to inspect the body (for usage stamping, deep validation,
	// etc.) should ReadAll first and pass a reqbody.NewReader here — a
	// *bytes.Reader stays reachable from the transport for the whole
	// response. Run sets Body to nil once the upstream answers.
	Body io.Reader

	// ContentLength is the inbound request's Content-Length, applied to
	// the upstream request explicitly because a non-seekable streamed Body
	// gives http.NewRequest no length to infer — without it the forward
	// degrades to chunked transfer. <= 0 means unknown.
	ContentLength int64

	// Headers is the inbound header set with relay-internal headers
	// already stripped (X-WR-*, Cookie, hop-by-hop).
	// Authorization is set separately via UpstreamAuth — do not include
	// it here.
	Headers http.Header

	// HostBaseURL is the resolved upstream base URL from the Host row.
	HostBaseURL string

	// UpstreamAuth is the verbatim "Authorization" value the caller
	// supplied (typically "Bearer …"). Forwarded as-is.
	UpstreamAuth string

	// Counters are what the request must fit: an authed caller's own counters and the proxy ceiling, or the anonymous ceiling alone. Empty disables limiting.
	Counters []limits.Counter

	// Pricing prices the tokens settled against budgets; nil when the request routed to no priced binding.
	Pricing *pricing.Pricing

	// Extractor parses the upstream response body for usage logging.
	// nil disables extraction (post-flight still commits the reservation).
	Extractor TokenExtractor

	// ModelName is for logging only — proxy mode does not consult the
	// catalog Model row.
	ModelName string

	// Lifecycle is the per-request shared context, constructed by the
	// handler before Run. Post-flight observers see it via the registered
	// lifecycle observers (Finalize). Optional — nil skips observer dispatch.
	Lifecycle *lifecycle.Context
}

// Result is what the handler streams back to the caller.
type Result struct {
	Status  int
	Headers http.Header
	// Body MUST be Closed by the caller. Closing triggers the post-
	// flight goroutine (if it hasn't run already).
	Body io.ReadCloser
	// Abort closes the upstream response under a Body still being read, from any goroutine: the read fails, and post-flight still waits for Body's own Close, which the reader makes.
	Abort func()
}

// Pipeline is the orchestrator. Constructed once at boot; Run() is
// goroutine-safe.
type Pipeline struct {
	// Limiter holds and settles Request.Counters; required when a request has any.
	Limiter   limits.Limiter
	Lifecycle *lifecycle.Registry
	Client    *http.Client
	Logger    *slog.Logger
}

// New constructs a Pipeline with a sensible http.Client default. The
// client has no overall timeout — streaming responses can take minutes.
// Use net/http transport timeouts for hop-level safety instead.
//
// lifecycle is optional; pass nil if post-flight observers aren't wired
// in this deployment (tests, minimal smoke).
func New(limiter limits.Limiter, registry *lifecycle.Registry, logger *slog.Logger) *Pipeline {
	return &Pipeline{
		Limiter:   limiter,
		Lifecycle: registry,
		Client:    &http.Client{},
		Logger:    logger,
	}
}

// ErrNoUpstreamAuth signals the handler forgot to populate UpstreamAuth.
var ErrNoUpstreamAuth = errors.New("proxy: upstream authorization required")

// ErrLimiterMissing signals a request with counters reached a Pipeline with no limiter; refused rather than forwarded unmetered.
var ErrLimiterMissing = errors.New("proxy: limiter not configured")

// BodyError is returned by a Request.Body reader that refuses the inbound body while it streams upstream, so the upstream received it incomplete. The failure is the caller's request, not the upstream; Kind names it in telemetry.
type BodyError struct {
	Kind string
	Err  error
}

func (e *BodyError) Error() string { return e.Err.Error() }
func (e *BodyError) Unwrap() error { return e.Err }

// RefusingBody is a Request.Body that can refuse the inbound body after the upstream has answered, when Run has already returned. Post-flight asks it: a refused request is recorded under the refusal's kind and its reservation settled as cancelled, not as a success.
type RefusingBody interface {
	Refusal() *BodyError
}

// Run forwards one proxy-mode request. The returned Body MUST be Closed.
func (p *Pipeline) Run(ctx context.Context, req *Request) (res *Result, err error) {
	// Failure telemetry: any error return (missing auth, rate-limit
	// rejection, upstream network failure) fires a post-flight observer
	// event so failed proxy requests aren't invisible to usage tracking.
	// Success returns nil err here and fires post-flight on Body.Close.
	// A reservation taken before the failure is refunded.
	var reservation limits.Reservation
	refusing, _ := req.Body.(RefusingBody)
	defer func() {
		if err != nil {
			// A refused body is the cause even when the transport reports what it led to: a read timeout also cancels the request's context.
			if refusing != nil {
				if refused := refusing.Refusal(); refused != nil {
					err = refused
				}
			}
			// Released before the error returns, so a caller retrying at once never finds its own hold.
			if reservation != nil {
				reservation.Release()
			}
			go p.fireFailure(req, err)
		}
	}()

	if req.UpstreamAuth == "" {
		return nil, ErrNoUpstreamAuth
	}

	if len(req.Counters) > 0 {
		if p.Limiter == nil {
			return nil, ErrLimiterMissing
		}
		// The 429 mapping is the caller's.
		if reservation, err = p.Limiter.Admit(ctx, req.Counters, limits.Usage{}); err != nil {
			return nil, err
		}
	}

	url := strings.TrimRight(req.HostBaseURL, "/") + req.Path
	upstream, err := http.NewRequestWithContext(ctx, req.Method, url, req.Body)
	if err != nil {
		return nil, fmt.Errorf("proxy: build upstream request: %w", err)
	}
	// Forward caller's headers verbatim (handler already applied the
	// inbound strip). Caller's Authorization is set last so it cannot
	// be clobbered by an accidental header collision in req.Headers.
	for k, vs := range req.Headers {
		for _, v := range vs {
			upstream.Header.Add(k, v)
		}
	}
	upstream.Header.Set("Authorization", req.UpstreamAuth)
	if req.ContentLength > 0 {
		upstream.ContentLength = req.ContentLength
	}

	req.Lifecycle.MarkUpstreamStart()
	resp, err := p.Client.Do(upstream)
	req.Body = nil
	if err != nil {
		return nil, fmt.Errorf("proxy: upstream call: %w", err)
	}

	// Tee for post-flight token extraction. Same pattern as pipeline:
	// io.TeeReader copies bytes to a collector as the caller reads. The
	// first-byte reader stamps the upstream TTFT + response-end marks.
	var collected bytes.Buffer
	tee := io.TeeReader(resp.Body, &collected)
	if req.Lifecycle != nil {
		req.Lifecycle.Streamed = strings.Contains(
			strings.ToLower(resp.Header.Get("Content-Type")), "event-stream")
	}
	pfTriggered := &sync.Once{}
	status := resp.StatusCode
	// The tee'd copy arrives however the upstream compressed it — the
	// proxy forwards Accept-Encoding verbatim, so unlike the pipeline
	// path nothing decompressed it. Post-flight decodes by this value.
	respEncoding := resp.Header.Get("Content-Encoding")

	body := &postFlightReadCloser{
		Reader: req.Lifecycle.FirstByteReader(tee),
		closer: func() error {
			pfTriggered.Do(func() {
				// End = response closed. Stamped here, not in the post-flight
				// goroutine — see pipeline.makeResult for the rationale.
				req.Lifecycle.MarkEnd()
				go p.runPostFlight(req, reservation, collected.Bytes(), status, respEncoding, refusing)
			})
			return resp.Body.Close()
		},
	}

	return &Result{
		Status:  resp.StatusCode,
		Headers: resp.Header,
		Body:    body,
		Abort:   func() { _ = resp.Body.Close() },
	}, nil
}

func (p *Pipeline) runPostFlight(req *Request, res limits.Reservation, body []byte, status int, encoding string, refusing RefusingBody) {
	// post_flight_seconds spans this whole goroutine — extraction, observer
	// fan-out, and the commit RTT below — not just the Finalize fan-out.
	pfStart := time.Now()
	defer func() { metrics.RecordPostFlightTotal(time.Since(pfStart)) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Decode ONCE, before every consumer: the extractor would otherwise
	// parse compressed bytes (silent zero tokens on br/zstd upstreams) and
	// payload capture would store — and worse, MaxBytes-clip — compressed
	// data, leaving records permanently undecodable. Decode failure keeps
	// the raw bytes: a garbled capture beats no capture.
	if len(body) > 0 {
		if decoded, err := contentcoding.Decode(body, encoding); err == nil {
			body = decoded
		} else if p.Logger != nil {
			p.Logger.Warn("proxy: response body decode failed; keeping raw bytes",
				"err", err, "content_encoding", encoding)
		}
	}

	if p.Logger != nil {
		reqID := ""
		if req.Lifecycle != nil {
			reqID = req.Lifecycle.RequestID
		}
		p.Logger.Debug("proxy: post-flight enter",
			"request_id", reqID,
			"status", status,
			"body_bytes", len(body),
		)
	}

	tokens := chargedTokens(req.Lifecycle, req.Extractor, body)

	// Finalize before the commit: usage/payload hooks need the response body.
	// lc carries persistent identity; the event carries this-request's outcome.
	ev := &lifecycle.PostFlightEvent{Status: status, ResponseBody: body}
	var refused *BodyError
	if refusing != nil {
		refused = refusing.Refusal()
	}
	if refused != nil {
		ev.ErrorKind, ev.ErrorMessage = refused.Kind, refused.Error()
	}
	if p.Lifecycle != nil && req.Lifecycle != nil {
		p.Lifecycle.Finalize(ctx, req.Lifecycle, ev)
	}

	// Body consumed. Drop the reference so the buffered response is GC-eligible
	// during the slower Valkey commit below. The commit runs after Finalize:
	// post-flight is off the client path, so ordering it last is safe.
	body = nil

	if res == nil {
		return
	}
	if refused != nil {
		res.Release()
		return
	}
	res.Settle(limits.Usage{Tokens: tokens, CostNanos: costNanos(req.Pricing, req.Lifecycle, tokens)})
}

// costNanos prices tokens at pr, at the service tier the upstream reported; 0 when unpriced.
func costNanos(pr *pricing.Pricing, lc *lifecycle.Context, tokens sdkusage.Tokens) int64 {
	if pr == nil || len(tokens) == 0 {
		return 0
	}
	tier := ""
	if s, ok := lc.ResponseSummary(); ok {
		tier = s.ServiceTier
	}
	n, _, ok := pr.CostNanosForServiceTier(tokens, tier)
	if !ok {
		return 0
	}
	return n
}

// fireFailure emits a post-flight observer event for a proxy request
// that never produced a response body. Own goroutine — the caller is
// about to write an error response. No reservation commit: no success.
func (p *Pipeline) fireFailure(req *Request, runErr error) {
	if p.Lifecycle == nil || req.Lifecycle == nil {
		return
	}
	kind, status := classifyFailure(runErr)
	req.Lifecycle.MarkEnd()
	p.Lifecycle.Finalize(context.Background(), req.Lifecycle, &lifecycle.PostFlightEvent{
		Status:       status,
		ErrorKind:    kind,
		ErrorMessage: runErr.Error(),
	})
}

// classifyFailure maps a proxy Run error to a telemetry category. Proxy
// reaches upstream in one attempt with the caller's own credential, so
// the failure set is narrower than the pipeline's.
func classifyFailure(err error) (kind string, status int) {
	var refused *limits.Refused
	var body *BodyError
	switch {
	case errors.As(err, &body):
		return body.Kind, 0
	case errors.Is(err, ErrNoUpstreamAuth):
		return "no_upstream_auth", 0
	case errors.As(err, &refused):
		// Refused before the upstream call → status 0.
		return "rate_limited", 0
	case errors.Is(err, pkgratelimit.ErrUnavailable):
		return "kv_unavailable", 0
	case errors.Is(err, ErrLimiterMissing):
		return "limiter_missing", 0
	case errors.Is(err, context.Canceled):
		return "client_canceled", 0
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", 0
	default:
		// Build-request or upstream Do() failure (network, DNS, TLS).
		return "upstream_error", 0
	}
}

type postFlightReadCloser struct {
	io.Reader
	closer func() error
}

func (r *postFlightReadCloser) Close() error {
	if r.closer != nil {
		return r.closer()
	}
	return nil
}
