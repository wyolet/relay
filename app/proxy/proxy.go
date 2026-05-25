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
//   - System-owned rate limits only (inference-api-proxy for authed,
//     inference-api-proxy-anonymous for anonymous), keyed by relay-key
//     hash or client IP respectively.
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

	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/pkg/usage"
)

// TokenExtractor extracts upstream usage from a response body. The
// inference handler picks the right extractor by inbound endpoint
// (OpenAI shape for /v1/chat/completions, Anthropic shape for
// /v1/messages) and passes it in.
type TokenExtractor interface {
	ExtractTokens(body []byte) pkgusage.Tokens
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
	// etc.) should ReadAll first and pass a *bytes.Reader here — proxy
	// stays shape-blind either way.
	Body io.Reader

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

	// RateScope is the limiter bucket subject — the relay-key hash for
	// authed, the client IP for anonymous. Empty disables limiting.
	RateScope string
	// Rules is the resolved rule set (system-owned for proxy mode).
	Rules []pkgratelimit.Rule

	// Extractor parses the upstream response body for usage logging.
	// nil disables extraction (post-flight still commits the reservation).
	Extractor TokenExtractor

	// ModelName is for logging only — proxy mode does not consult the
	// catalog Model row.
	ModelName string

	// Lifecycle is the per-request shared context, constructed by the
	// handler before Run. Post-flight observers see it via the registered
	// PostFlightHook chain. Optional — nil skips hook dispatch.
	Lifecycle *lifecycle.Context
}

// Result is what the handler streams back to the caller.
type Result struct {
	Status  int
	Headers http.Header
	// Body MUST be Closed by the caller. Closing triggers the post-
	// flight goroutine (if it hasn't run already).
	Body io.ReadCloser
}

// Pipeline is the orchestrator. Constructed once at boot; Run() is
// goroutine-safe.
type Pipeline struct {
	Limiter   *pkgratelimit.Limiter
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
func New(limiter *pkgratelimit.Limiter, registry *lifecycle.Registry, logger *slog.Logger) *Pipeline {
	return &Pipeline{
		Limiter:   limiter,
		Lifecycle: registry,
		Client:    &http.Client{},
		Logger:    logger,
	}
}

// ErrNoUpstreamAuth signals the handler forgot to populate UpstreamAuth.
var ErrNoUpstreamAuth = errors.New("proxy: upstream authorization required")

// Run forwards one proxy-mode request. The returned Body MUST be Closed.
func (p *Pipeline) Run(ctx context.Context, req *Request) (res *Result, err error) {
	// Failure telemetry: any error return (missing auth, rate-limit
	// rejection, upstream network failure) fires a post-flight observer
	// event so failed proxy requests aren't invisible to usage tracking.
	// Success returns nil err here and fires post-flight on Body.Close.
	defer func() {
		if err != nil {
			go p.fireFailure(req, err)
		}
	}()

	if req.UpstreamAuth == "" {
		return nil, ErrNoUpstreamAuth
	}

	var reservation *pkgratelimit.Reservation
	if p.Limiter != nil && len(req.Rules) > 0 && req.RateScope != "" {
		reservation, err = p.Limiter.Reserve(ctx, req.RateScope, req.Rules)
		if err != nil {
			return nil, err // 429 envelope handled by caller
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

	req.Lifecycle.MarkUpstreamStart()
	resp, err := p.Client.Do(upstream)
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

	body := &postFlightReadCloser{
		Reader: req.Lifecycle.FirstByteReader(tee),
		closer: func() error {
			pfTriggered.Do(func() {
				go p.runPostFlight(req, reservation, collected.Bytes(), status)
			})
			return resp.Body.Close()
		},
	}

	return &Result{
		Status:  resp.StatusCode,
		Headers: resp.Header,
		Body:    body,
	}, nil
}

func (p *Pipeline) runPostFlight(req *Request, res *pkgratelimit.Reservation, body []byte, status int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if p.Logger != nil {
		reqID := ""
		if req.Lifecycle != nil {
			reqID = req.Lifecycle.RequestID
		}
		p.Logger.Info("proxy: post-flight enter",
			"request_id", reqID,
			"status", status,
			"body_bytes", len(body),
		)
	}

	var tokens pkgusage.Tokens
	if req.Extractor != nil {
		tokens = req.Extractor.ExtractTokens(body)
	}

	if res != nil && p.Limiter != nil {
		obs := pkgratelimit.Observations{Tokens: map[string]int64(tokens)}
		if err := p.Limiter.Commit(ctx, res, obs); err != nil && p.Logger != nil {
			p.Logger.Warn("proxy: limiter commit failed",
				"err", err, "scope", req.RateScope)
		}
	}

	// Fan out to lifecycle observers. lc carries persistent identity;
	// the event carries this-request's outcome.
	if p.Lifecycle != nil && req.Lifecycle != nil {
		req.Lifecycle.MarkEnd()
		ev := &lifecycle.PostFlightEvent{
			Status:       status,
			ResponseBody: body,
		}
		p.Lifecycle.FirePostFlight(ctx, req.Lifecycle, ev)
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.Lifecycle.FirePostFlight(ctx, req.Lifecycle, &lifecycle.PostFlightEvent{
		Status:       status,
		ErrorKind:    kind,
		ErrorMessage: runErr.Error(),
	})
}

// classifyFailure maps a proxy Run error to a telemetry category. Proxy
// reaches upstream in one attempt with the caller's own credential, so
// the failure set is narrower than the pipeline's.
func classifyFailure(err error) (kind string, status int) {
	var exceeded *pkgratelimit.ExceededError
	switch {
	case errors.Is(err, ErrNoUpstreamAuth):
		return "no_upstream_auth", 0
	case errors.As(err, &exceeded):
		return "rate_limited", http.StatusTooManyRequests
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
