// Package pipeline orchestrates a single authenticated inference request:
// reserve rate-limit budget → pick a host key → call the upstream via the
// shape-specific Adapter → stream the response back → record success +
// commit the reservation in a detached post-flight goroutine.
//
// The pipeline is deliberately ignorant of: JSON shapes, the catalog
// snapshot, HTTP routing, identity, and the cross-shape transform. All
// resolution work happens upstream of Run(); the pipeline consumes
// pre-resolved primitives. Anonymous / passthrough traffic is handled by
// a separate package (planned: app/proxy), not by a branch in here.
//
// Performance contract (per CLAUDE.md):
//   - Post-flight (Limiter.Commit + Selector.RecordSuccess) runs in a
//     detached goroutine — never blocks the response.
//   - No PostgreSQL calls. No catalog reloads.
//   - Streaming bodies are streamed; no full-body buffering on the hot
//     path. The token extractor sees a body copy via a tee.
package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/policy"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/pkg/usage"
)

// Adapter is the shape-specific surface the pipeline calls into. One
// implementation lives in app/api/openai, another in app/api/anthropic.
// Adapter is the seam between "orchestration" (this package) and "wire
// format" (the api packages).
type Adapter interface {
	// Call issues the upstream HTTP request. baseURL is the Host's
	// BaseURL; apiKey is the cleartext credential the Selector picked.
	// body is the byte-equivalent forward; hdr carries headers the
	// handler decided to forward. The returned *http.Response is
	// expected to have a streaming Body the pipeline copies through.
	Call(ctx context.Context, baseURL, apiKey string, body []byte, hdr http.Header) (*http.Response, error)

	// ExtractTokens parses an upstream response body and returns the
	// usage breakdown. For streaming responses the pipeline passes the
	// full body collected via a tee, so the adapter need only know the
	// non-streaming shape; SSE-aware extraction is the adapter's
	// implementation detail when it matters.
	ExtractTokens(body []byte) pkgusage.Tokens

	// Retryable reports whether an upstream HTTP response should cause
	// a key rotation + retry. Status codes like 401/403/429/5xx are
	// typically retryable; the adapter decides because the meaning of
	// the same status code differs by upstream (some 200s carry an
	// error envelope in the body).
	Retryable(resp *http.Response) (retry bool, kind keypool.FailureKind, retryAfter time.Duration)
}

// Request is the pre-resolved input. Build it in app/routing or the
// handler. The pipeline does no resolution of its own.
type Request struct {
	Body    []byte
	Headers http.Header

	HostBaseURL string
	Adapter     Adapter

	// Policy carries KeySelection algo + Meta.Name used as kv scope.
	// Kept as a pointer (not flattened to strings) because Selector.Pick
	// expects the full Policy.
	Policy *policy.Policy

	// Keys are the ordered candidate set. Empty rejected with ErrNoKeys.
	// Anonymous traffic is served by a separate package, not this one.
	Keys []*hostkey.HostKey

	// RateScope is the kv key tag for limit grouping (typically the
	// Policy slug). Rules are the resolved []pkgratelimit.Rule the
	// handler built from policy + ratelimit + per-key caps.
	RateScope string
	Rules     []pkgratelimit.Rule

	// ModelName is for logging + emit. No catalog imports — just a string.
	ModelName string

	// MaxAttempts caps retries. 0 falls back to defaultMaxAttempts.
	MaxAttempts int

	// OnSuccess fires from the post-flight goroutine once the body has
	// been fully streamed and tokens extracted. Optional. Receives the
	// extracted tokens and the keyHash that served the request.
	OnSuccess func(tokens pkgusage.Tokens, keyHash string)
}

// Result is what the handler streams back to the caller.
type Result struct {
	Status  int
	Headers http.Header
	// Body MUST be Closed by the caller. Closing triggers the post-flight
	// goroutine (if it hasn't run already).
	Body    io.ReadCloser
	KeyHash string
}

// Pipeline is the orchestrator. Constructed once at boot; Run() is
// goroutine-safe.
type Pipeline struct {
	Limiter  *pkgratelimit.Limiter
	Selector *keypool.Selector
	Logger   *slog.Logger
}

const defaultMaxAttempts = 3

var (
	// ErrNoKeys signals a Request arrived with zero candidate keys.
	// Callers should reject earlier; reaching this means a bug.
	ErrNoKeys = errors.New("pipeline: no candidate keys")

	// ErrAllKeysExhausted signals every candidate was tried and failed
	// in a retryable way. Maps to 503.
	ErrAllKeysExhausted = errors.New("pipeline: all keys exhausted")

	// ErrAdapterMissing signals the Request omitted an Adapter. Caller bug.
	ErrAdapterMissing = errors.New("pipeline: adapter is nil")
)

// Run orchestrates one request. Returns a streaming Result on success;
// the caller MUST Close the Result.Body to release the upstream
// connection AND trigger post-flight emit.
func (p *Pipeline) Run(ctx context.Context, req *Request) (*Result, error) {
	if req.Adapter == nil {
		return nil, ErrAdapterMissing
	}
	if len(req.Keys) == 0 {
		return nil, ErrNoKeys
	}

	maxAttempts := req.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	if maxAttempts > len(req.Keys) {
		maxAttempts = len(req.Keys)
	}

	// Reserve rate-limit budget. A failure here is a 429 before we even
	// dial upstream — return it as a typed error the handler maps.
	var (
		reservation *pkgratelimit.Reservation
		err         error
	)
	if p.Limiter != nil && len(req.Rules) > 0 {
		reservation, err = p.Limiter.Reserve(ctx, req.RateScope, req.Rules)
		if err != nil {
			return nil, err // handler maps pkgratelimit.ExceededError → 429
		}
	}

	// Retry loop: pick → call → on retryable error, rotate key.
	var (
		excluded []*hostkey.HostKey
		chosen   *hostkey.HostKey
		resp     *http.Response
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		chosen, err = p.Selector.PickWithExclude(ctx, req.Policy, req.Keys, excluded)
		if err != nil {
			break
		}

		resp, err = req.Adapter.Call(ctx, req.HostBaseURL, chosen.Resolved, req.Body, req.Headers)
		if err == nil && resp != nil && !shouldRetry(req.Adapter, resp) {
			// Success path (or non-retryable upstream error — return as-is).
			return p.makeResult(req, reservation, chosen, resp), nil
		}

		// Retryable: classify, record failure, drain body, exclude this
		// key on the next pick.
		retry, kind, retryAfter := classify(req.Adapter, resp, err)
		if resp != nil {
			drainAndClose(resp.Body)
		}
		if !retry {
			break
		}
		p.Selector.RecordFailure(ctx, chosen.KeyHash, kind, retryAfter)
		excluded = append(excluded, chosen)
		chosen = nil // don't carry forward
	}

	// Reservation never committed — let it expire by TTL. Limiter is
	// designed to be lossy on this path.
	if err == nil {
		err = ErrAllKeysExhausted
	}
	return nil, err
}

// shouldRetry asks the Adapter whether a successful HTTP response is
// actually retryable (e.g. 200 with an error envelope, 429, 401).
func shouldRetry(a Adapter, resp *http.Response) bool {
	if resp == nil {
		return false
	}
	retry, _, _ := a.Retryable(resp)
	return retry
}

// classify maps an attempt outcome to (retry?, failureKind, retryAfter).
func classify(a Adapter, resp *http.Response, callErr error) (bool, keypool.FailureKind, time.Duration) {
	if callErr != nil {
		// Network-level error — short cooldown, retry on next key.
		return true, keypool.FailureNetwork, 0
	}
	return a.Retryable(resp)
}

func (p *Pipeline) makeResult(
	req *Request,
	res *pkgratelimit.Reservation,
	chosen *hostkey.HostKey,
	resp *http.Response,
) *Result {
	// Tee the body so post-flight can read what the caller read. Buffer
	// is bounded by the caller's read pace — io.Pipe blocks when the
	// reader stops, so we use a tee-into-buffer + a goroutine that
	// reads the buffered side after Close.
	//
	// For typical chat completions the response is in the tens of KB;
	// for long streaming sessions the buffer grows with the stream. If
	// memory pressure becomes real we can swap this for a chunked
	// scanner that the adapter consumes online — but that's a later
	// optimisation.
	var collected bytes.Buffer
	tee := io.TeeReader(resp.Body, &collected)

	pfTriggered := &sync.Once{}
	postFlight := func() {
		pfTriggered.Do(func() {
			go p.runPostFlight(req, res, chosen, collected.Bytes())
		})
	}

	body := &postFlightReadCloser{
		Reader: tee,
		closer: func() error {
			postFlight()
			return resp.Body.Close()
		},
	}

	return &Result{
		Status:  resp.StatusCode,
		Headers: resp.Header,
		Body:    body,
		KeyHash: chosen.KeyHash,
	}
}

// runPostFlight commits the reservation and records success. Detached
// from the request goroutine; failures are logged but never surface to
// the caller.
func (p *Pipeline) runPostFlight(
	req *Request,
	res *pkgratelimit.Reservation,
	chosen *hostkey.HostKey,
	body []byte,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tokens := req.Adapter.ExtractTokens(body)

	if res != nil && p.Limiter != nil {
		obs := pkgratelimit.Observations{Tokens: map[string]int64(tokens)}
		if err := p.Limiter.Commit(ctx, res, obs); err != nil && p.Logger != nil {
			p.Logger.Warn("pipeline: limiter commit failed",
				"err", err, "scope", req.RateScope)
		}
	}

	if p.Selector != nil && chosen != nil {
		p.Selector.RecordSuccess(ctx, chosen.KeyHash)
	}

	if req.OnSuccess != nil {
		req.OnSuccess(tokens, chosen.KeyHash)
	}
}

// postFlightReadCloser wraps an io.Reader with a custom Close. Reads
// pass through; Close runs the supplied closer fn.
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

// drainAndClose reads and discards a response body so the underlying
// HTTP connection can be reused. Called on retried attempts before
// closing.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}

// RetryAfterHeader is a small helper for adapters classifying 429s.
// Returns the parsed duration from a Retry-After header, or 0 if
// unparseable. Exported so adapters in app/api/* can use it.
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

// String returns a one-line summary of a Request useful in logs. Does
// not include the body.
func (r *Request) String() string {
	policyName := ""
	if r.Policy != nil {
		policyName = r.Policy.Meta.Name
	}
	return fmt.Sprintf("pipeline.Request{model=%q policy=%q host=%s keys=%d rules=%d}",
		r.ModelName, policyName, r.HostBaseURL, len(r.Keys), len(r.Rules))
}
