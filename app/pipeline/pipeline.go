// Package pipeline orchestrates one inference request: reserve inbound
// budget, acquire an upstream key (and its upstream reservation) via
// policy.Service, call the adapter, stream back, commit in post-flight.
//
// The pipeline is ignorant of catalog/snapshot, wire shapes, and policy
// internals. All policy-shaped work routes through policy.Service.
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

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgusage "github.com/wyolet/relay/pkg/usage"
)

// Adapter is the wire-shape seam: app/api/openai, app/api/anthropic.
type Adapter interface {
	Call(ctx context.Context, baseURL, apiKey string, body []byte, hdr http.Header) (*http.Response, error)
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

	// MaxAttempts caps retries (0 → defaultMaxAttempts).
	MaxAttempts int

	// OnSuccess fires from the post-flight goroutine after tokens are
	// extracted. Optional.
	OnSuccess func(tokens pkgusage.Tokens, keyHash string)
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
	Policy *policy.Service
	Logger *slog.Logger
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
func (p *Pipeline) Run(ctx context.Context, req *Request) (*Result, error) {
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
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
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

		resp, err = req.Adapter.Call(ctx, req.HostBaseURL, acq.Key.Resolved, req.Body, req.Headers)
		if err == nil && resp != nil && !shouldRetry(req.Adapter, resp) {
			return p.makeResult(req, inbound, acq, resp), nil
		}

		retry, kind, retryAfter := classify(req.Adapter, resp, err)
		if resp != nil {
			drainAndClose(resp.Body)
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
	return nil, err
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
	// Tee the body so post-flight can read what the caller read.
	var collected bytes.Buffer
	tee := io.TeeReader(resp.Body, &collected)

	pfTriggered := &sync.Once{}
	postFlight := func() {
		pfTriggered.Do(func() {
			go p.runPostFlight(req, inbound, acq, collected.Bytes())
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
		KeyHash: acq.KeyHash(),
	}
}

func (p *Pipeline) runPostFlight(
	req *Request,
	inbound *pkgratelimit.Reservation,
	acq *policy.Acquisition,
	body []byte,
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

	if req.OnSuccess != nil {
		req.OnSuccess(tokens, acq.KeyHash())
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
