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
	"net"
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
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// Adapter is the wire-shape seam, implemented by app/adapter.specAdapter.
//
// upstreamModel is the resolved upstream model name (snapshot.Upstream());
// stream reports whether the caller requested a streamed response. Most
// shapes ignore both (model + stream live in the request body), but shapes
// that encode them in the URL path — Gemini's generateContent vs
// streamGenerateContent — need them to build the upstream URL.
type Adapter interface {
	Call(ctx context.Context, baseURL, apiKey string, body []byte, hdr http.Header, upstreamModel string, stream, oauth bool) (*http.Response, error)
	ExtractTokens(body []byte) sdkusage.Tokens
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

	// UpstreamModel is the resolved upstream wire name (routing's
	// Plan.UpstreamModel() — the snapshot's upstream name, or the alias
	// verbatim override when resolution matched a declared alias). Passed
	// to Adapter.Call for shapes that put the model in the URL path.
	UpstreamModel string

	// Stream reports whether the caller requested a streamed response, passed
	// to Adapter.Call for shapes that select a distinct stream URL.
	Stream bool

	// MaxAttempts caps retries (0 → defaultMaxAttempts).
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

// Pipeline orchestrates requests. All policy-shaped work is delegated
// to Policy (the Service). Construct once at boot.
type Pipeline struct {
	Policy    *policy.Service
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
		excluded     []*hostkey.HostKey
		acq          *policy.Acquisition
		resp         *http.Response
		keyValue     string // current secret for acq.Key; overridden on a heal-retry
		attempts     int    // distinct keys tried (a same-key retry doesn't count)
		connAttempts int    // consecutive dial failures against the host (any key)
		// Last upstream response observed during retry. Carried into the
		// final error so callers see *why* upstream rejected (otherwise
		// "all keys exhausted" hides the actual auth/quota/server message).
		lastStatus int
		lastBody   string
	)
loop:
	for {
		if acq == nil {
			if attempts >= maxAttempts {
				break loop
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
				acq = nil
				attempts++
				continue
			}
			if err != nil {
				break loop
			}
			attempts++
			keyValue = acq.Key.Resolved
		}
		if req.Lifecycle != nil {
			req.Lifecycle.Attempts = attempts
		}

		// An OAuth-kind credential authenticates differently upstream (Bearer +
		// vendor beta header vs an API-key header); the adapter picks the auth
		// variant. Determined per acquired key, so a failover from an oauth key
		// to an api key (or vice versa) authenticates each correctly.
		oauth := acq != nil && acq.Key != nil && acq.Key.Spec.ValueFrom.Kind == hostkey.ValueKindOAuth
		req.Lifecycle.MarkUpstreamStart()
		resp, err = req.Adapter.Call(ctx, req.HostBaseURL, keyValue, req.Body, req.Headers, req.UpstreamModel, req.Stream, oauth)
		if err == nil && resp != nil && !shouldRetry(req.Adapter, resp) {
			return p.makeResult(req, inbound, acq, resp), nil
		}

		retry, kind, retryAfter := classify(req.Adapter, resp, err)
		if resp != nil {
			lastStatus = resp.StatusCode
			lastBody = readBodyExcerpt(resp, 512)
		}

		// Dial failure: the host is unreachable, not the key bad. Don't trip
		// the breaker and don't fail over keys (they share the baseURL) —
		// re-dial the same host with a short backoff, then report unreachable.
		if kind == keypool.FailureUpstreamUnreachable {
			connAttempts++
			if connAttempts < maxConnAttempts {
				if werr := sleepCtx(ctx, connBackoff(connAttempts)); werr != nil {
					err = werr
					break loop
				}
				continue // reuse acq + its reservation; re-call the same host
			}
			err = &UpstreamUnreachableError{Host: hostSlug, Attempts: connAttempts, Cause: err}
			break loop
		}

		moreCandidates := retry && attempts < maxAttempts && untried(req.Keys, excluded, acq.Key) > 0
		action, fresh := p.handleFailure(ctx, acq, retry, kind, retryAfter, moreCandidates)
		switch action {
		case actRetrySame:
			keyValue = fresh // reuse acq + its reservation; re-call with healed secret
		case actNextKey:
			excluded = append(excluded, acq.Key)
			acq = nil
		default: // actStop
			break loop
		}
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

type attemptAction int

const (
	actNextKey   attemptAction = iota // fail over to the next candidate key
	actRetrySame                      // retry the same key with the fresh secret
	actStop                           // stop the loop; surface the upstream error
)

// handleFailure decides what to do after a failed Adapter.Call. Non-retryable
// failures stop without touching the breaker (a 400 isn't the key's fault).
// For retryable failures it consults the KeyAgent when set; otherwise it falls
// back to legacy behavior (release the key → fail over). The breaker-tripping
// Release is issued on every path that abandons the key (Next / agent-Fail),
// and skipped on a heal-retry (the key is good again).
func (p *Pipeline) handleFailure(
	ctx context.Context,
	acq *policy.Acquisition,
	retry bool,
	kind keypool.FailureKind,
	retryAfter time.Duration,
	moreCandidates bool,
) (attemptAction, string) {
	if !retry {
		return actStop, ""
	}
	if p.KeyAgent != nil && acq != nil && acq.Key != nil {
		switch v, fresh := p.KeyAgent.OnFailure(ctx, acq.Key.Meta.ID, kind, moreCandidates); v {
		case VerdictRetry:
			return actRetrySame, fresh // healed → reuse the key, no breaker trip
		case VerdictFail:
			p.Policy.Release(ctx, acq, kind, retryAfter)
			return actStop, ""
		}
		// VerdictNext falls through to the failover release below.
	}
	p.Policy.Release(ctx, acq, kind, retryAfter)
	return actNextKey, ""
}

// untried counts keys neither excluded nor currently held — candidates the
// loop could still fail over to.
func untried(all, excluded []*hostkey.HostKey, current *hostkey.HostKey) int {
	skip := make(map[string]struct{}, len(excluded)+1)
	if current != nil {
		skip[current.Meta.ID] = struct{}{}
	}
	for _, k := range excluded {
		if k != nil {
			skip[k.Meta.ID] = struct{}{}
		}
	}
	n := 0
	for _, k := range all {
		if k != nil {
			if _, ok := skip[k.Meta.ID]; !ok {
				n++
			}
		}
	}
	return n
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

	// The host answered — record reachability (off the latency path).
	if p.HostHealth != nil && req.Host != nil {
		p.HostHealth.Reachable(ctx, req.Host.Meta.ID)
	}

	// Fan out to lifecycle observers. lc carries persistent identity;
	// the event carries this-request's outcome. Observers see both.
	if p.Lifecycle != nil && req.Lifecycle != nil {
		req.Lifecycle.HostKeyID = acq.KeyHash()
		req.Lifecycle.MarkEnd()
		ev := &lifecycle.PostFlightEvent{
			Status:       status,
			ResponseBody: body,
		}
		p.Lifecycle.Finalize(ctx, req.Lifecycle, ev)
	}
}

// fireFailure emits a post-flight observer event for a request that
// never produced a response body. Runs in its own goroutine (the caller
// is about to write an error response — telemetry must not block it).
// No rate-limit commit / RecordSuccess: there was no success.
func (p *Pipeline) fireFailure(req *Request, runErr error) {
	// Record host reachability regardless of lifecycle wiring: a dial failure
	// means unreachable; an upstream-status failure means the host answered
	// (the keys/quota are the problem, not connectivity).
	if p.HostHealth != nil && req.Host != nil {
		var unreachable *UpstreamUnreachableError
		var upstream *UpstreamFailureError
		switch {
		case errors.As(runErr, &unreachable):
			p.HostHealth.Unreachable(context.Background(), req.Host.Meta.ID, runErr.Error())
		case errors.As(runErr, &upstream):
			p.HostHealth.Reachable(context.Background(), req.Host.Meta.ID)
		}
	}

	if p.Lifecycle == nil || req.Lifecycle == nil {
		return
	}
	kind, status := classifyFailure(runErr)
	req.Lifecycle.MarkEnd()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p.Lifecycle.Finalize(ctx, req.Lifecycle, &lifecycle.PostFlightEvent{
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
	var exceeded *pkgratelimit.ExceededError
	switch {
	case errors.As(err, &unreachable):
		return "upstream_unreachable", 0
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
