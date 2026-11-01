// Package pipeline orchestrates one inference request over a route the plans already resolved: hold the caller's counters, walk the route's hops in order, pick a credential inside a hop with the key pool's selection and breakers, hold that credential's upstream counters, call the adapter, stream back, and settle in post-flight.
//
// The pipeline is ignorant of the catalog, wire shapes and the policy ladder: which counters apply and which credentials may serve come from the route (app/plan), counting from a limits.Limiter.
//
// Expected store calls per request: one limiter script for the caller's counters and one per credential tried for its upstream counters (none for a side with no counters), plus the key pool's breaker reads per pick. Post-flight: one settle per held reservation and, only when the picked record changes on success, the breaker's success write.
package pipeline

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	sdkusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Adapter is the wire-shape seam, implemented by app/adapter.specAdapter.
//
// upstreamModel is the resolved upstream model name (snapshot.Upstream());
// stream reports whether the caller requested a streamed response. Most
// shapes ignore both (model + stream live in the request body), but shapes
// that encode them in the URL path — Gemini's generateContent vs
// streamGenerateContent — need them to build the upstream URL.
type Adapter interface {
	// hostPath is the host's upstream-path override (Host.Spec.Path): used
	// verbatim when non-nil (explicit "" appends nothing to baseURL); nil
	// keeps the adapter's shape default.
	Call(ctx context.Context, baseURL string, hostPath *string, apiKey string, body []byte, hdr http.Header, upstreamModel string, stream, oauth bool) (*http.Response, error)
	ExtractTokens(body []byte) sdkusage.Tokens
	Retryable(resp *http.Response) (retry bool, kind keypool.FailureKind, retryAfter time.Duration)
}

// CallTimeouter is the optional capability of an Adapter that caps one call end to end, body read included; 0 means no cap. The runner sets that cap on the call context it derives anyway, so the adapter need not derive a second one.
type CallTimeouter interface {
	CallTimeout(stream bool) time.Duration
}

// Request is the pre-resolved input. Built by the handler/router.
type Request struct {
	// Body is the upstream wire body. Run sets it to nil once an upstream response is accepted: no retry can follow, and a long stream must not pin it.
	Body    []byte
	Headers http.Header

	Adapter Adapter

	Model    *model.Model
	Provider string

	// Hops are the route's pools in failover order, each with the credentials that may serve and the counters each one's request must fit. No slot in any hop → ErrNoKeys. Every hop must speak the Adapter's wire shape: the body is built once.
	Hops []plan.RouteHop

	ModelName string

	// UpstreamModel is the resolved upstream wire name (routing's
	// Plan.UpstreamModel() — the snapshot's upstream name, or the alias
	// verbatim override when resolution matched a declared alias). Passed
	// to Adapter.Call for shapes that put the model in the URL path.
	UpstreamModel string

	// Stream reports whether the caller requested a streamed response, passed
	// to Adapter.Call for shapes that select a distinct stream URL.
	Stream bool

	// MaxAttempts caps the credentials called upstream (0 → defaultMaxAttempts).
	MaxAttempts int

	// Lifecycle is the per-request shared context, constructed by the
	// handler before Run. Post-flight observers see it via the registered
	// lifecycle observers (Finalize). Optional — when nil, post-flight skips
	// dispatch (legacy callers / tests).
	Lifecycle *lifecycle.Context
}

// Result is what the handler streams back. Caller MUST Close Body —
// that triggers post-flight.
type Result struct {
	Status  int
	Headers http.Header
	Body    io.ReadCloser
	KeyHash string
}

// Pipeline orchestrates requests. Construct once at boot.
type Pipeline struct {
	// Limiter holds and settles the route's counters. Nil refuses every request (ErrLimiterMissing): limits fail closed.
	Limiter limits.Limiter
	// Selector picks a credential inside a hop and reads its breaker; nil takes the first candidate.
	Selector *keypool.Selector
	// PostSelector writes breaker transitions after an attempt, on a store admission never waits on; nil uses Selector.
	PostSelector *keypool.Selector

	Lifecycle *lifecycle.Registry
	Logger    *slog.Logger

	// KeyAgent decides what to do when an upstream key fails (fail over /
	// retry-with-refreshed-secret / stop). Nil = legacy behavior. The loop
	// only consults it for retryable failures.
	KeyAgent KeyAgent

	// HostHealth records observed upstream reachability for the admin UI.
	// Written only from the detached post-flight goroutines, never on the
	// latency path. Nil disables health recording.
	HostHealth HostHealth
}

// HostHealth records observed host reachability. Implemented by
// app/hosthealth.Recorder; wired in the composition root.
type HostHealth interface {
	// Reachable marks the host as having answered (any HTTP response).
	Reachable(ctx context.Context, hostID string)
	// Unreachable marks the host as having failed to connect, with an error excerpt.
	Unreachable(ctx context.Context, hostID, errMsg string)
}

const defaultMaxAttempts = 3

// maxConnAttempts bounds how many times the pipeline re-dials the SAME host
// after a connection-establishment failure (dial refused / DNS / TLS). A dial
// failure is host-level, not key-level, so failing over to other keys is
// pointless (they share the baseURL). We absorb a transient blip with a short
// backoff, then report the host unreachable.
const maxConnAttempts = 3

var (
	ErrNoKeys           = errors.New("pipeline: no candidate keys")
	ErrAllKeysExhausted = errors.New("pipeline: all keys exhausted")
	ErrAdapterMissing   = errors.New("pipeline: adapter is nil")
	ErrLimiterMissing   = errors.New("pipeline: limiter is nil")
)

// UpstreamLimitedError means no credential of the route had room: every one tried was refused by an upstream counter (a host, account or credential limit) or sat in its account's cooldown. RetryAfter is the earliest any of them frees up.
type UpstreamLimitedError struct {
	RetryAfter time.Duration
	// Refused is the last upstream counter that refused, nil when only cooldowns stood in the way.
	Refused *limits.Refused
}

func (e *UpstreamLimitedError) Error() string {
	return "pipeline: every credential for this model is over an upstream limit"
}

// CallerLimitedError is a refusal on one of the caller's own counters: the request ends there, naming the rung and policy that refused.
type CallerLimitedError struct{ Refused *limits.Refused }

func (e *CallerLimitedError) Error() string { return e.Refused.Error() }

func (e *CallerLimitedError) Unwrap() error { return e.Refused }

// maxUpstreamErrorBody bounds the retained upstream error body. Generous enough that no provider's error JSON is truncated (they run to a few KiB at most with a validation trace), bounded because this buffer outlives the response and a hostile upstream must not be able to pin arbitrary memory per failed request.
const maxUpstreamErrorBody = 64 << 10

// maxTeePresize caps the response buffer sized up front from Content-Length. The header is the upstream's claim, not bytes received, so a larger one grows the buffer as the body arrives instead.
const maxTeePresize = 64 << 10

// newTeeBuffer returns the buffer a buffered response is copied into, sized up front when contentLength is known and at most maxTeePresize.
func newTeeBuffer(contentLength int64) *bytes.Buffer {
	b := &bytes.Buffer{}
	if contentLength > 0 && contentLength <= maxTeePresize {
		b.Grow(int(contentLength))
	}
	return b
}

// errorExcerpt bounds how much of the retained body reaches a log line or an error string.
const errorExcerpt = 512

// UpstreamFailureError wraps ErrAllKeysExhausted with the last upstream response — status, headers and body — so handlers can relay what actually went wrong instead of a generic "all upstream keys failed". Only the LAST attempt is kept: the earlier candidates failed the same way, and the caller acts on one verdict.
type UpstreamFailureError struct {
	Status int
	Header http.Header
	Body   []byte
	Cause  error
}

func (e *UpstreamFailureError) Error() string {
	body := bodyExcerpt(e.Body, errorExcerpt)
	if body == "" {
		body = "(empty body)"
	}
	return fmt.Sprintf("upstream returned %d: %s", e.Status, body)
}

func (e *UpstreamFailureError) Unwrap() error { return e.Cause }

// bodyExcerpt trims b to a short summary for logs and error strings, leaving the retained body itself untouched so it can still be forwarded verbatim.
func bodyExcerpt(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// readBodyCapped reads up to max bytes from resp.Body, drains the rest, and returns them. Honors Content-Encoding: gzip (some providers compress error bodies) so the retained bytes are the decoded payload — which is also why the encoding header must not be forwarded alongside them.
func readBodyCapped(resp *http.Response, max int) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	defer resp.Body.Close()
	var src io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(resp.Body)
		if err == nil {
			defer gr.Close()
			src = gr
		}
	}
	buf := make([]byte, max)
	n, _ := io.ReadFull(src, buf)
	if n == 0 {
		return nil
	}
	_, _ = io.Copy(io.Discard, src)
	return buf[:n]
}

func shouldRetry(a Adapter, resp *http.Response) bool {
	if resp == nil {
		return false
	}
	retry, _, _ := a.Retryable(resp)
	return retry
}

func classify(a Adapter, resp *http.Response, callErr error) (bool, keypool.FailureKind, time.Duration) {
	if callErr != nil {
		if errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
			return false, keypool.FailureNetwork, 0 // caller went away — not the key's fault
		}
		if isConnError(callErr) {
			return true, keypool.FailureUpstreamUnreachable, 0
		}
		return true, keypool.FailureNetwork, 0
	}
	return a.Retryable(resp)
}

// isConnError reports whether err is a connection-establishment failure — the
// upstream was never reached (connection refused, no route, DNS failure, TLS
// handshake). Such errors are host/baseURL problems, not key problems. A
// post-connect read timeout is NOT a conn error (it falls back to
// FailureNetwork) since the host was reachable.
func isConnError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Op=="dial" covers refused/no-route/TLS-handshake — the connect phase.
		return opErr.Op == "dial"
	}
	return false
}

// connBackoff returns the wait before re-dialing the host. attempt is the
// 1-based count of consecutive dial failures: 100ms, 200ms, capped at 2s.
func connBackoff(attempt int) time.Duration {
	d := 100 * time.Millisecond << (attempt - 1)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// sleepCtx waits d or until ctx is cancelled, returning ctx.Err() on cancel.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// UpstreamUnreachableError signals every attempt to reach the host failed at
// the connection-establishment phase — a likely baseURL misconfiguration or a
// down upstream, NOT a bad key. Distinct from UpstreamFailureError (which
// carries an actual upstream HTTP status the keys were rejected with).
type UpstreamUnreachableError struct {
	Host     string
	Attempts int
	Cause    error
}

func (e *UpstreamUnreachableError) Error() string {
	return fmt.Sprintf("upstream host %q unreachable after %d attempt(s): %v", e.Host, e.Attempts, e.Cause)
}

func (e *UpstreamUnreachableError) Unwrap() error { return e.Cause }

func (p *Pipeline) makeResult(req *Request, st *runState, resp *http.Response, call *upstreamCall) *Result {
	status := resp.StatusCode
	requestBytes := len(req.Body)
	req.Body = nil
	if req.Lifecycle != nil {
		req.Lifecycle.Streamed = req.Stream
		// Stamp the outcome so the metrics stream observer (whose Fill fan-out
		// a stream session skips) can read it at end-of-stream.
		req.Lifecycle.ResponseStatus = status
	}

	// The first-byte reader stamps the upstream TTFT + response-end marks as
	// the caller drains the tee. What the tee feeds differs by mode:
	//
	//   - streamed: tee into a StreamSession, which frames the SSE bytes and
	//     extracts usage/payload/tokens INCREMENTALLY, retaining only the
	//     current partial frame — never the whole stream. This is the memory
	//     fix: a 1000-frame stream no longer sits buffered until post-flight.
	//   - buffered (non-streamed, or streamed with no observers registered):
	//     tee into a full bytes.Buffer that post-flight re-parses. The buffer
	//     escapes into the post-flight goroutine, so it stays unpooled.
	var (
		collected *bytes.Buffer
		teeDst    io.Writer
		sess      *lifecycle.StreamSession
	)
	if req.Stream && p.Lifecycle != nil {
		sess = p.Lifecycle.NewStreamSession(req.Lifecycle) // nil when no factories
	}
	if sess != nil {
		req.Lifecycle.SetStreamSession(sess)
		teeDst = sess
	} else {
		collected = newTeeBuffer(resp.ContentLength)
		teeDst = collected
	}
	read := &readState{r: io.TeeReader(resp.Body, teeDst)}

	// Finish attaches usage/payload and stashes rate-limit tokens on the
	// Context. Idempotent: the echo response-writer finishes it early (to
	// splice usage into the terminal frame); this is a no-op then.
	finish := func() (body []byte, sessFrames int) {
		if sess != nil {
			sess.Finish()
			return nil, sess.Frames()
		}
		return collected.Bytes(), 0
	}

	pfTriggered := &sync.Once{}
	body := &postFlightReadCloser{
		Reader: req.Lifecycle.FirstByteReader(read),
		closer: func() error {
			var err error
			pfTriggered.Do(func() {
				// End = response closed. Stamped here, not in the post-flight
				// goroutine: bookkeeping time (rate-limit commits, observer
				// fan-out) is relay_post_flight_seconds, never duration_ms /
				// relay_overhead_seconds.
				req.Lifecycle.MarkEnd()
				if read.done {
					body, frames := finish()
					go p.runPostFlight(req, st, body, frames, requestBytes, status, read.eof)
					err = resp.Body.Close()
					call.release()
					return
				}
				// The caller stopped early; the usage frame is usually still
				// ahead in the response, so read the rest off the response path.
				go func() {
					complete := drainTail(read, call)
					body, frames := finish()
					_ = resp.Body.Close()
					call.release()
					p.runPostFlight(req, st, body, frames, requestBytes, status, complete)
				}()
			})
			return err
		},
	}

	return &Result{
		Status:  status,
		Headers: resp.Header,
		Body:    body,
		KeyHash: st.cur.slot.Credential.KeyHash,
	}
}

func (p *Pipeline) runPostFlight(
	req *Request,
	st *runState,
	body []byte,
	sessFrames int,
	requestBytes int,
	status int,
	complete bool,
) {
	// post_flight_seconds spans this whole goroutine — extraction, observer
	// fan-out, and the commit RTTs below — not just the Finalize fan-out.
	pfStart := time.Now()
	defer func() { metrics.RecordPostFlightTotal(time.Since(pfStart)) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tokens := chargedTokens(req.Lifecycle, req.Adapter, body)
	// A response cut short before its end may never have reported usage;
	// the upstream still did the work, so the budgets are charged a floor
	// rather than nothing. Usage records keep only what was reported.
	limitTokens := tokens
	if !complete {
		frames := sessFrames
		if body != nil {
			frames = countSSEFrames(body)
		}
		limitTokens = withUnreportedFloor(tokens, requestBytes, frames)
	}

	// Finalize before the commits: usage/payload hooks need the response body.
	// lc carries persistent identity; the event carries this-request's outcome.
	if p.Lifecycle != nil && req.Lifecycle != nil {
		p.Lifecycle.Finalize(ctx, req.Lifecycle, &lifecycle.PostFlightEvent{
			Status:       status,
			ResponseBody: body,
		})
	}

	// Body consumed. Drop the reference so the buffered response is GC-eligible
	// during the slower Valkey commit RTTs below — before this reorder it
	// survived all of them. Commits run after Finalize: post-flight is off the
	// client path, so ordering them last is safe.
	body = nil

	st.settle(limits.Usage{Tokens: limitTokens, CostNanos: st.cost(req.Lifecycle, limitTokens)})
	if st.cur.cand.SuccessNeedsWrite() {
		if sel := p.postSelector(); sel != nil {
			sel.RecordSuccess(ctx, st.cur.slot.Credential.KeyHash)
		}
	}

	// The host answered — record reachability (off the latency path).
	if p.HostHealth != nil {
		p.HostHealth.Reachable(ctx, st.cur.hop.Host.Meta.ID)
	}
}

// ResponseSummarizer is the optional capability of an Adapter that reads the canonical Summary of a buffered response, so post-flight decodes the body once for the rate-limit commit and usage alike. false means it has no translator for the body; the caller then uses ExtractTokens.
type ResponseSummarizer interface {
	SummarizeResponse(body []byte) (v1.Summary, bool)
}

// chargedTokens returns the tokens the rate-limit commit charges: the ones the stream session extracted incrementally, else the buffered body's Summary's, decoded once and shared with the usage hook. A Summary without tokens (a stream that ended on an error event or before its terminal frame) can sit beside usage the upstream did report; those tokens were served, so ExtractTokens is asked for them.
func chargedTokens(lc *lifecycle.Context, a Adapter, body []byte) sdkusage.Tokens {
	var tokens sdkusage.Tokens
	if t, ok := lc.StreamTokens(); ok {
		tokens = sdkusage.Tokens(t)
	} else if sum, ok := responseSummary(lc, a, body); ok {
		tokens = sum.Tokens
	}
	if len(tokens) == 0 && len(body) > 0 {
		tokens = a.ExtractTokens(body)
	}
	return tokens
}

// responseSummary returns the buffered response's Summary: the one the handler recorded while translating it, else the one a decodes now, which is recorded on lc so the usage hook reuses it.
func responseSummary(lc *lifecycle.Context, a Adapter, body []byte) (v1.Summary, bool) {
	if s, ok := lc.ResponseSummary(); ok {
		return s, true
	}
	rs, ok := a.(ResponseSummarizer)
	if !ok {
		return v1.Summary{}, false
	}
	s, ok := rs.SummarizeResponse(body)
	if ok {
		lc.SetResponseSummary(s)
	}
	return s, ok
}

// fireFailure emits a post-flight observer event for a request that
// never produced a response body. Runs in its own goroutine (the caller
// is about to write an error response — telemetry must not block it).
// No rate-limit commit / RecordSuccess: there was no success.
func (p *Pipeline) fireFailure(req *Request, h *host.Host, runErr error) {
	// Record host reachability regardless of lifecycle wiring: a dial failure
	// means unreachable; an upstream-status failure means the host answered
	// (the keys/quota are the problem, not connectivity).
	if p.HostHealth != nil && h != nil {
		var unreachable *UpstreamUnreachableError
		var upstream *UpstreamFailureError
		switch {
		case errors.As(runErr, &unreachable):
			p.HostHealth.Unreachable(context.Background(), h.Meta.ID, runErr.Error())
		case errors.As(runErr, &upstream):
			p.HostHealth.Reachable(context.Background(), h.Meta.ID)
		}
	}

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

// classifyFailure maps a Run error to a machine-readable category and the
// upstream status to record (0 when upstream was never reached). Kinds are
// telemetry categories for slicing — they don't mirror HTTP status codes.
func classifyFailure(err error) (kind string, status int) {
	var upstream *UpstreamFailureError
	var unreachable *UpstreamUnreachableError
	var refused *limits.Refused
	var limited *UpstreamLimitedError
	switch {
	case errors.As(err, &unreachable):
		return "upstream_unreachable", 0
	case errors.As(err, &upstream):
		return "upstream_error", upstream.Status
	case errors.As(err, &refused), errors.As(err, &limited):
		// Refused before any upstream call → status 0
		// (the kind carries the 429 meaning; Status is the upstream status).
		return "rate_limited", 0
	case errors.Is(err, pkgratelimit.ErrUnavailable):
		return "kv_unavailable", 0
	case errors.Is(err, ErrNoKeys):
		return "no_keys", 0
	case errors.Is(err, ErrAllKeysExhausted):
		return "keys_exhausted", 0
	case errors.Is(err, ErrAdapterMissing):
		return "adapter_missing", 0
	case errors.Is(err, ErrLimiterMissing):
		return "limiter_missing", 0
	case errors.Is(err, context.Canceled):
		return "client_canceled", 0
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", 0
	default:
		return "error", 0
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

// RetryAfterHeader parses a Retry-After header. Exported for adapters.
func RetryAfterHeader(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

func (r *Request) String() string {
	return fmt.Sprintf("pipeline.Request{model=%q hops=%d}", r.ModelName, len(r.Hops))
}
