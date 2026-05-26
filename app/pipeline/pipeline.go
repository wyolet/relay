// Package pipeline orchestrates one inference request: reserve inbound
// budget, acquire an upstream key (and its upstream reservation) via
// policy.Service, call the adapter, stream back, commit in post-flight.
//
// The pipeline is ignorant of catalog/snapshot, wire shapes, and policy
// internals. All policy-shaped work routes through policy.Service.
package pipeline

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/pkg/usage"
)

// Adapter is the wire-shape seam, implemented by app/adapter.specAdapter.
//
// upstreamModel is the resolved upstream model name (snapshot.Upstream());
// stream reports whether the caller requested a streamed response. Most
// shapes ignore both (model + stream live in the request body), but shapes
// that encode them in the URL path — Gemini's generateContent vs
// streamGenerateContent — need them to build the upstream URL.
type Adapter interface {
	Call(ctx context.Context, baseURL, apiKey string, body []byte, hdr http.Header, upstreamModel string, stream bool) (*http.Response, error)
	ExtractTokens(body []byte) pkgusage.Tokens
	Retryable(resp *http.Response) (retry bool, kind keypool.FailureKind, retryAfter time.Duration)
}

// Request is the pre-resolved input. Built by the handler/router.
type Request struct {
	Body    []byte
	Headers http.Header

	HostBaseURL string
	Adapter     Adapter

	Policy   *policy.Policy
	Model    *model.Model
	Host     *host.Host
	Provider string

	// Keys is the ordered candidate set. Empty → ErrNoKeys.
	Keys []*hostkey.HostKey

	ModelName string

	// UpstreamModel is the resolved upstream model name (snapshot.Upstream()),
	// passed to Adapter.Call for shapes that put the model in the URL path.
	UpstreamModel string

	// Stream reports whether the caller requested a streamed response, passed
	// to Adapter.Call for shapes that select a distinct stream URL.
	Stream bool

	// MaxAttempts caps retries (0 → defaultMaxAttempts).
	MaxAttempts int

	// Lifecycle is the per-request shared context, constructed by the
	// handler before Run. Post-flight observers see it via the registered
	// PostFlightHook chain. Optional — when nil, post-flight skips hook
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

// Pipeline orchestrates requests. All policy-shaped work is delegated
// to Policy (the Service). Construct once at boot.
type Pipeline struct {
	Policy    *policy.Service
	Lifecycle *lifecycle.Registry
	Logger    *slog.Logger
}

const defaultMaxAttempts = 3

var (
	ErrNoKeys           = errors.New("pipeline: no candidate keys")
	ErrAllKeysExhausted = errors.New("pipeline: all keys exhausted")
	ErrAdapterMissing   = errors.New("pipeline: adapter is nil")
	ErrPolicyMissing    = errors.New("pipeline: policy service is nil")
)

// Run orchestrates one request. Caller MUST Close the returned
// Result.Body to release the connection and trigger post-flight.
func (p *Pipeline) Run(ctx context.Context, req *Request) (res *Result, err error) {
	// Failure telemetry: any error return (guard, reservation, routing
	// of keys, all-keys-exhausted, upstream failure) fires a post-flight
	// observer event so failed requests aren't invisible to usage
	// tracking. Success returns nil err here and fires post-flight later
	// on Body.Close instead — the two are mutually exclusive.
	defer func() {
		if err != nil {
			go p.fireFailure(req, err)
		}
	}()

	if req.Adapter == nil {
		return nil, ErrAdapterMissing
	}
	if len(req.Keys) == 0 {
		return nil, ErrNoKeys
	}
	if p.Policy == nil {
		return nil, ErrPolicyMissing
	}

	maxAttempts := req.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	if maxAttempts > len(req.Keys) {
		maxAttempts = len(req.Keys)
	}

	modelSlug, hostSlug := "", ""
	if req.Model != nil {
		modelSlug = req.Model.Meta.Name
	}
	if req.Host != nil {
		hostSlug = req.Host.Meta.Name
	}

	inbound, err := p.Policy.ReserveInbound(ctx, req.Policy, req.Provider, modelSlug, hostSlug)
	if err != nil {
		return nil, err // handler maps ExceededError → 429
	}

	var (
		excluded []*hostkey.HostKey
		acq      *policy.Acquisition
		resp     *http.Response
		// Last upstream response observed during retry. Carried into the
		// final error so callers see *why* upstream rejected (otherwise
		// "all keys exhausted" hides the actual auth/quota/server message).
		lastStatus int
		lastBody   string
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if req.Lifecycle != nil {
			req.Lifecycle.Attempts = attempt + 1
		}
		acq, err = p.Policy.Acquire(ctx, policy.AcquireInput{
			Policy:   req.Policy,
			Keys:     req.Keys,
			Excluded: excluded,
			Model:    req.Model,
			Host:     req.Host,
			Provider: req.Provider,
		})
		if errors.Is(err, policy.ErrSaturated) {
			if acq != nil && acq.Key != nil {
				excluded = append(excluded, acq.Key)
			}
			continue
		}
		if err != nil {
			break
		}

		req.Lifecycle.MarkUpstreamStart()
		resp, err = req.Adapter.Call(ctx, req.HostBaseURL, acq.Key.Resolved, req.Body, req.Headers, req.UpstreamModel, req.Stream)
		if err == nil && resp != nil && !shouldRetry(req.Adapter, resp) {
			return p.makeResult(req, inbound, acq, resp), nil
		}

		retry, kind, retryAfter := classify(req.Adapter, resp, err)
		if resp != nil {
			lastStatus = resp.StatusCode
			lastBody = readBodyExcerpt(resp, 512)
		}
		if !retry {
			break
		}
		p.Policy.Release(ctx, acq, kind, retryAfter)
		excluded = append(excluded, acq.Key)
		acq = nil
	}

	if err == nil {
		err = ErrAllKeysExhausted
	}
	if errors.Is(err, ErrAllKeysExhausted) && lastStatus != 0 {
		err = &UpstreamFailureError{Status: lastStatus, Body: lastBody, Cause: err}
	}
	return nil, err
}

// UpstreamFailureError wraps ErrAllKeysExhausted with the last upstream
// status + body excerpt so handlers can surface what actually went wrong
// (otherwise the caller just sees "all upstream keys failed" with no
// context — auth? quota? bad model? unknown).
type UpstreamFailureError struct {
	Status int
	Body   string
	Cause  error
}

func (e *UpstreamFailureError) Error() string {
	body := e.Body
	if body == "" {
		body = "(empty body)"
	}
	return fmt.Sprintf("upstream returned %d: %s", e.Status, body)
}

func (e *UpstreamFailureError) Unwrap() error { return e.Cause }

// readBodyExcerpt reads up to max bytes from resp.Body, drains the rest,
// and returns the read bytes as a string. Honors Content-Encoding: gzip
// (Anthropic compresses error bodies) so the excerpt is human-readable
// rather than raw deflate. Empty if body is nil or unreadable.
func readBodyExcerpt(resp *http.Response, max int) string {
	if resp == nil || resp.Body == nil {
		return ""
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
		return ""
	}
	_, _ = io.Copy(io.Discard, src)
	return strings.TrimSpace(string(buf[:n]))
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
		return true, keypool.FailureNetwork, 0
	}
	return a.Retryable(resp)
}

func (p *Pipeline) makeResult(
	req *Request,
	inbound *pkgratelimit.Reservation,
	acq *policy.Acquisition,
	resp *http.Response,
) *Result {
	// Tee the body so post-flight can read what the caller read. The
	// first-byte reader stamps the upstream TTFT + response-end marks as
	// the caller drains the tee.
	var collected bytes.Buffer
	tee := io.TeeReader(resp.Body, &collected)
	if req.Lifecycle != nil {
		req.Lifecycle.Streamed = req.Stream
	}

	pfTriggered := &sync.Once{}
	status := resp.StatusCode
	postFlight := func() {
		pfTriggered.Do(func() {
			go p.runPostFlight(req, inbound, acq, collected.Bytes(), status)
		})
	}

	body := &postFlightReadCloser{
		Reader: req.Lifecycle.FirstByteReader(tee),
		closer: func() error {
			postFlight()
			return resp.Body.Close()
		},
	}

	return &Result{
		Status:  resp.StatusCode,
		Headers: resp.Header,
		Body:    body,
		KeyHash: acq.KeyHash(),
	}
}

func (p *Pipeline) runPostFlight(
	req *Request,
	inbound *pkgratelimit.Reservation,
	acq *policy.Acquisition,
	body []byte,
	status int,
) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tokens := req.Adapter.ExtractTokens(body)
	obs := pkgratelimit.Observations{Tokens: map[string]int64(tokens)}

	if err := p.Policy.CommitInbound(ctx, inbound, obs); err != nil && p.Logger != nil {
		p.Logger.Warn("pipeline: inbound commit failed", "err", err)
	}
	if err := p.Policy.Commit(ctx, acq, obs); err != nil && p.Logger != nil {
		p.Logger.Warn("pipeline: upstream commit failed", "err", err)
	}
	p.Policy.RecordSuccess(ctx, acq)

	// Fan out to lifecycle observers. lc carries persistent identity;
	// the event carries this-request's outcome. Observers see both.
	if p.Lifecycle != nil && req.Lifecycle != nil {
		req.Lifecycle.HostKeyID = acq.KeyHash()
		req.Lifecycle.MarkEnd()
		ev := &lifecycle.PostFlightEvent{
			Status:       status,
			ResponseBody: body,
		}
		p.Lifecycle.FirePostFlight(ctx, req.Lifecycle, ev)
	}
}

// fireFailure emits a post-flight observer event for a request that
// never produced a response body. Runs in its own goroutine (the caller
// is about to write an error response — telemetry must not block it).
// No rate-limit commit / RecordSuccess: there was no success.
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

// classifyFailure maps a Run error to a machine-readable category and the
// upstream status to record (0 when upstream was never reached). Kinds are
// telemetry categories for slicing — they don't mirror HTTP status codes.
func classifyFailure(err error) (kind string, status int) {
	var upstream *UpstreamFailureError
	var exceeded *pkgratelimit.ExceededError
	switch {
	case errors.As(err, &upstream):
		return "upstream_error", upstream.Status
	case errors.As(err, &exceeded):
		// Reservation rejected before any upstream call → status 0
		// (the kind carries the 429 meaning; Status is the upstream status).
		return "rate_limited", 0
	case errors.Is(err, ErrNoKeys):
		return "no_keys", 0
	case errors.Is(err, ErrAllKeysExhausted):
		return "keys_exhausted", 0
	case errors.Is(err, ErrAdapterMissing):
		return "adapter_missing", 0
	case errors.Is(err, ErrPolicyMissing):
		return "policy_missing", 0
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

func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
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
	policyName := ""
	if r.Policy != nil {
		policyName = r.Policy.Meta.Name
	}
	return fmt.Sprintf("pipeline.Request{model=%q policy=%q host=%s keys=%d}",
		r.ModelName, policyName, r.HostBaseURL, len(r.Keys))
}
