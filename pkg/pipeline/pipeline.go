package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/keypool"
	"github.com/wyolet/relay/pkg/limit"
	"github.com/wyolet/relay/pkg/provider"
	"github.com/wyolet/relay/pkg/reqid"
	"github.com/wyolet/relay/pkg/transport"
	"github.com/wyolet/relay/pkg/usage"
)

var (
	ErrNoInboundMessage = errors.New("pipeline: no inbound message on Channel.In")
	ErrAttemptsExhausted = errors.New("pipeline: all attempts exhausted")
)

const defaultMaxAttempts = 3
const shortRateLimitThreshold = 5 * time.Second

// RunOptions configures a Run invocation.
type RunOptions struct {
	Provider    *configstore.Provider
	Pool        *configstore.Pool
	Model       *configstore.Model
	Secrets     []*configstore.Secret
	Selector    *keypool.Selector
	Outbound    provider.Outbound
	MaxAttempts int // 0 → 3

	// Limiter and Rules enable rate limiting. If either is nil/empty, rate
	// limiting is skipped (preserves M2 behavior for configs without limits).
	// Rules should be pre-resolved by the caller for Pool+Model scope.
	// Secret-level rules are M4+ work.
	Limiter *limit.Limiter
	Rules   []configstore.ResolvedRule
}

// Run reads the inbound Message from ch.In and orchestrates upstream calls
// with retry/failover. It closes ch.Out before returning. Pre-first-byte
// retry only: once a non-error first response chunk is forwarded, the
// response is committed.
func Run(ctx context.Context, ch *transport.Channel, opts RunOptions) (retErr error) {
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

	// Build lifecycle. Fields are stamped incrementally throughout Run.
	lc := &usage.Lifecycle{
		RequestID:    reqid.From(ctx),
		Attribution:  reqid.Attribution(ctx),
		StartedAt:    time.Now(),
		InstanceID:   usage.InstanceID(),
		RelayVersion: usage.RelayVersion(),
		Metrics:      map[string]int64{},
	}
	lc.SetSpan(usage.SpanFromContext(ctx))

	// emit fires on every exit path (normal and panic-recovered).
	// Registered first → runs second (LIFO). Panic recovery below runs first
	// and sets TerminatedBy so emit doesn't overwrite it.
	defer func() {
		lc.EndedAt = time.Now()
		lc.Metrics["total_ms"] = lc.EndedAt.Sub(lc.StartedAt).Milliseconds()
		if lc.TerminatedBy == "" {
			lc.TerminatedBy = classifyTermination(ctx, retErr)
		}
		usage.Record(ctx, lc)
	}()

	// Panic recovery registered second → runs first (LIFO). Recovers panics,
	// stamps TerminatedRelayError, then returns via retErr so emit fires next.
	defer func() {
		if r := recover(); r != nil {
			lc.TerminatedBy = usage.TerminatedRelayError
			retErr = errors.New("pipeline: recovered panic")
		}
	}()

	defer close(ch.Out)

	// Read inbound message.
	var inboundMsg *transport.Message
	select {
	case msg, ok := <-ch.In:
		if !ok {
			return ErrNoInboundMessage
		}
		inboundMsg = msg
	case <-ctx.Done():
		return ctx.Err()
	}

	// Extract model from inbound if set (best-effort; opts.Model is canonical).
	if opts.Model != nil {
		lc.Model = opts.Model.Metadata.Name
	}
	if opts.Provider != nil {
		lc.Provider = opts.Provider.Metadata.Name
	}
	if opts.Pool != nil {
		lc.Pool = opts.Pool.Metadata.Name
	}

	// Rate limiting: Reserve before the retry loop; Commit via defer regardless of path.
	var tokensSeen int64
	if opts.Limiter != nil && len(opts.Rules) > 0 {
		res, err := opts.Limiter.Reserve(ctx, opts.Rules)
		if err != nil {
			var exceeded *limit.ExceededError
			if errors.As(err, &exceeded) {
				send429LimitEnvelope(ch.Out, exceeded)
			} else {
				sendGenericErrorEnvelope(ch.Out)
			}
			lc.TerminatedBy = usage.TerminatedRateLimited
			return err
		}
		defer func() {
			cancelled := ctx.Err() != nil
			commitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			opts.Limiter.Commit(commitCtx, res, limit.Observations{Tokens: tokensSeen, Cancelled: cancelled})
		}()
	}

	preUpstreamStart := time.Now()

	attempt := 0
	sameKeyAttempt := 0
	var chosenKey *configstore.Secret
	lastFailureKind := keypool.FailureKind(-1)
	var maxRetryAfter time.Duration
	var outboundPanicked atomic.Bool

	for attempt < maxAttempts {
		attempt++

		// Pick a key if we don't have one.
		if chosenKey == nil {
			var err error
			chosenKey, err = opts.Selector.Pick(ctx, opts.Provider, opts.Pool, opts.Model, opts.Secrets)
			if err != nil {
				if errors.Is(err, keypool.ErrNoHealthyKeys) {
					sendExhaustedEnvelope(ch.Out, keypool.FailureKind(-2), 0)
					lc.TerminatedBy = usage.TerminatedPoolExhausted
					return err
				}
				if errors.Is(err, keypool.ErrPoolOutOfCapacity) {
					sendPoolOutOfCapacityEnvelope(ch.Out)
					lc.TerminatedBy = usage.TerminatedPoolExhausted
					return err
				}
				lc.TerminatedBy = usage.TerminatedRelayError
				return err
			}
			sameKeyAttempt = 0
		}

		// Stamp pre_upstream_ms once (on first successful key pick).
		if _, ok := lc.Metrics["pre_upstream_ms"]; !ok {
			lc.Metrics["pre_upstream_ms"] = time.Since(preUpstreamStart).Milliseconds()
			lc.SecretHash = usage.SecretHash(chosenKey.Resolved)
		}

		secret := chosenKey
		attemptStart := time.Now()

		// Spawn outbound into intermediate channel. Recover panics: the outbound
		// implementation is expected to close inter (via its own defer) before
		// the panic propagates. We just catch the panic and send an error.
		inter := make(chan *transport.Message, 64)
		outboundErr := make(chan error, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					outboundPanicked.Store(true)
					outboundErr <- errors.New("outbound panic recovered")
				}
			}()
			outboundErr <- opts.Outbound.ChatCompletions(ctx, inboundMsg.Body, secret.Resolved, inter)
		}()

		// Read first message.
		var firstMsg *transport.Message
		select {
		case msg, ok := <-inter:
			if !ok || msg == nil {
				// Channel closed without message — treat as network failure.
				latencyMS := time.Since(attemptStart).Milliseconds()
				<-outboundErr
				opts.Selector.RecordFailure(ctx, secret.KeyHash, keypool.FailureNetwork, 0)
				lastFailureKind = keypool.FailureNetwork
				appendAttempt(lc, secret, "network_error", 0, latencyMS)
				slog.Default().Info("pipeline attempt",
					"attempt", attempt,
					"key_hash", secret.KeyHash,
					"classification", "network",
				)
				if sameKeyAttempt == 0 {
					sameKeyAttempt++
				} else {
					chosenKey = nil
					sameKeyAttempt = 0
				}
				continue
			}
			firstMsg = msg
		case <-ctx.Done():
			go drain(inter)
			return ctx.Err()
		}

		status := parseStatus(firstMsg.Headers["X-Relay-Status"])
		retryAfter := parseRetryAfter(firstMsg.Headers["Retry-After"])

		cls := classify(status)
		// Resolve 429 short vs long based on actual retryAfter.
		if cls == classRateLimit429Short && retryAfter > shortRateLimitThreshold {
			cls = classRateLimit429Long
		}

		switch cls {
		case classSuccess:
			opts.Selector.RecordSuccess(ctx, secret.KeyHash)
			slog.Default().Info("pipeline attempt",
				"attempt", attempt,
				"key_hash", secret.KeyHash,
				"status", status,
				"classification", "success",
			)
			peekTokens(firstMsg.Body, &tokensSeen)
			peekTokensFull(firstMsg.Body, lc)
			ttfb := time.Since(attemptStart).Milliseconds()
			lc.Metrics["upstream_ttfb_ms"] = ttfb
			ch.Out <- firstMsg
			for msg := range inter {
				select {
				case <-ctx.Done():
					go drain(inter)
					<-outboundErr
					latencyMS := time.Since(attemptStart).Milliseconds()
					appendAttempt(lc, secret, "success", status, latencyMS)
					return ctx.Err()
				default:
				}
				peekTokens(msg.Body, &tokensSeen)
				peekTokensFull(msg.Body, lc)
				ch.Out <- msg
			}
			<-outboundErr
			latencyMS := time.Since(attemptStart).Milliseconds()
			lc.Metrics["upstream_total_ms"] = latencyMS
			appendAttempt(lc, secret, "success", status, latencyMS)
			// If ctx was cancelled while draining inter, report that.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return nil

		case classAuth:
			latencyMS := time.Since(attemptStart).Milliseconds()
			go drain(inter)
			<-outboundErr
			opts.Selector.RecordFailure(ctx, secret.KeyHash, keypool.FailureAuth, 0)
			lastFailureKind = keypool.FailureAuth
			slog.Default().Info("pipeline attempt",
				"attempt", attempt,
				"key_hash", secret.KeyHash,
				"status", status,
				"classification", "auth",
			)
			appendAttempt(lc, secret, "http_4xx", status, latencyMS)
			chosenKey = nil
			sameKeyAttempt = 0

		case classRateLimit429Short:
			latencyMS := time.Since(attemptStart).Milliseconds()
			go drain(inter)
			<-outboundErr
			opts.Selector.RecordFailure(ctx, secret.KeyHash, keypool.FailureRateLimitShort, retryAfter)
			lastFailureKind = keypool.FailureRateLimitShort
			if retryAfter > maxRetryAfter {
				maxRetryAfter = retryAfter
			}
			slog.Default().Info("pipeline attempt",
				"attempt", attempt,
				"key_hash", secret.KeyHash,
				"status", status,
				"classification", "rate_limit_short",
				"retry_after_ms", retryAfter.Milliseconds(),
			)
			appendAttempt(lc, secret, "rate_limited", status, latencyMS)
			if retryAfter > 0 {
				select {
				case <-time.After(retryAfter):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			sameKeyAttempt++
			// chosenKey unchanged

		case classRateLimit429Long:
			latencyMS := time.Since(attemptStart).Milliseconds()
			go drain(inter)
			<-outboundErr
			opts.Selector.RecordFailure(ctx, secret.KeyHash, keypool.FailureRateLimitLong, retryAfter)
			lastFailureKind = keypool.FailureRateLimitLong
			if retryAfter > maxRetryAfter {
				maxRetryAfter = retryAfter
			}
			slog.Default().Info("pipeline attempt",
				"attempt", attempt,
				"key_hash", secret.KeyHash,
				"status", status,
				"classification", "rate_limit_long",
				"retry_after_ms", retryAfter.Milliseconds(),
			)
			appendAttempt(lc, secret, "rate_limited", status, latencyMS)
			chosenKey = nil
			sameKeyAttempt = 0

		case classServerError, classNetwork:
			latencyMS := time.Since(attemptStart).Milliseconds()
			go drain(inter)
			<-outboundErr
			kind := keypool.FailureServerError
			classification := "5xx"
			outcome := "http_5xx"
			if cls == classNetwork {
				kind = keypool.FailureNetwork
				classification = "network"
				outcome = "network_error"
			}
			opts.Selector.RecordFailure(ctx, secret.KeyHash, kind, 0)
			lastFailureKind = kind
			slog.Default().Info("pipeline attempt",
				"attempt", attempt,
				"key_hash", secret.KeyHash,
				"status", status,
				"classification", classification,
			)
			appendAttempt(lc, secret, outcome, status, latencyMS)
			if sameKeyAttempt == 0 {
				sameKeyAttempt++
			} else {
				chosenKey = nil
				sameKeyAttempt = 0
			}
		}
	}

	sendExhaustedEnvelope(ch.Out, lastFailureKind, maxRetryAfter)
	if outboundPanicked.Load() {
		lc.TerminatedBy = usage.TerminatedRelayError
	} else {
		lc.TerminatedBy = terminatedByFromFailureKind(lastFailureKind)
	}
	return ErrAttemptsExhausted
}

// appendAttempt adds an Attempt to lc, capped at AttemptsCap.
func appendAttempt(lc *usage.Lifecycle, secret *configstore.Secret, outcome string, status int, latencyMS int64) {
	if len(lc.Attempts) >= usage.AttemptsCap {
		return
	}
	lc.Attempts = append(lc.Attempts, usage.Attempt{
		SecretHash: usage.SecretHash(secret.Resolved),
		Outcome:    outcome,
		HTTPStatus: status,
		LatencyMS:  latencyMS,
	})
}

// classifyTermination maps ctx error / retErr to a TerminatedBy value.
func classifyTermination(ctx context.Context, err error) usage.TerminatedBy {
	if err == nil {
		return usage.TerminatedClean
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return usage.TerminatedUpstreamTimeout
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return usage.TerminatedClientCancel
	}
	if errors.Is(err, keypool.ErrNoHealthyKeys) || errors.Is(err, keypool.ErrPoolOutOfCapacity) {
		return usage.TerminatedPoolExhausted
	}
	if errors.Is(err, ErrAttemptsExhausted) {
		return usage.TerminatedUpstreamError
	}
	return usage.TerminatedRelayError
}

// terminatedByFromFailureKind maps the last failure kind after attempts exhausted.
func terminatedByFromFailureKind(kind keypool.FailureKind) usage.TerminatedBy {
	switch {
	case kind == keypool.FailureRateLimitShort || kind == keypool.FailureRateLimitLong:
		return usage.TerminatedUpstreamError
	case kind == keypool.FailureAuth:
		return usage.TerminatedUpstreamError
	default:
		return usage.TerminatedUpstreamError
	}
}

// peekTokensFull extracts full token block from message body into lifecycle.
func peekTokensFull(b []byte, lc *usage.Lifecycle) {
	if len(b) == 0 {
		return
	}
	tb, ok := limit.ParseTokensFull(b)
	if !ok {
		return
	}
	if tb.Total > lc.Tokens.Total {
		lc.Tokens = usage.TokenBlock{
			Prompt:     tb.Prompt,
			Completion: tb.Completion,
			Total:      tb.Total,
		}
	}
}

type responseClass int

const (
	classSuccess responseClass = iota
	classAuth
	classRateLimit429Short
	classRateLimit429Long
	classServerError
	classNetwork
)

func classify(status int) responseClass {
	switch {
	case status >= 200 && status < 300:
		return classSuccess
	case status == 401 || status == 403:
		return classAuth
	case status == 429:
		// caller resolves short vs long via retryAfter; we return short here as sentinel
		// The actual branching in Run() reads retryAfter separately.
		return classRateLimit429Short // placeholder; Run switches on this then checks retryAfter
	case status >= 500:
		return classServerError
	case status == 0:
		return classNetwork
	default:
		return classServerError
	}
}

// parseStatus converts the X-Relay-Status header string to int. 0 on failure.
func parseStatus(s string) int {
	if s == "" {
		return 0
	}
	n, _ := strconv.Atoi(s)
	return n
}

// parseRetryAfter parses Retry-After header value (seconds only; HTTP-date → 60s).
func parseRetryAfter(s string) time.Duration {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		// HTTP-date or unparseable — default 60s.
		return 60 * time.Second
	}
	return time.Duration(n) * time.Second
}

func drain(ch <-chan *transport.Message) {
	for range ch {
	}
}

const noHealthyKeysSentinel = keypool.FailureKind(-2)

func sendExhaustedEnvelope(out chan<- *transport.Message, kind keypool.FailureKind, retryAfter time.Duration) {
	var status, errType, code, msg string
	switch {
	case kind == noHealthyKeysSentinel:
		status = "503"
		errType = "upstream_error"
		code = "no_healthy_keys"
		msg = "no healthy keys available"
	case kind == keypool.FailureAuth:
		status = "502"
		errType = "upstream_error"
		code = "auth_failed"
		msg = "all keys exhausted: authentication failed"
	case kind == keypool.FailureRateLimitShort || kind == keypool.FailureRateLimitLong:
		status = "429"
		errType = "rate_limit_exceeded"
		code = "rate_limit_exceeded"
		msg = "all keys exhausted: rate limit exceeded"
	case kind == keypool.FailureServerError:
		status = "502"
		errType = "upstream_error"
		code = "upstream_5xx_exhausted"
		msg = "all keys exhausted: upstream server error"
	case kind == keypool.FailureNetwork:
		status = "502"
		errType = "upstream_error"
		code = "upstream_unavailable"
		msg = "all keys exhausted: upstream unavailable"
	default:
		// fallback
		status = "502"
		errType = "upstream_error"
		code = "pool_exhausted"
		msg = "all keys in pool exhausted"
	}

	headers := map[string]string{
		"X-Relay-Status": status,
		"Content-Type":   "application/json",
		"X-Relay-Final":  "true",
	}
	if (kind == keypool.FailureRateLimitShort || kind == keypool.FailureRateLimitLong) && retryAfter > 0 {
		headers["Retry-After"] = strconv.Itoa(int(retryAfter.Seconds()))
	}

	body := []byte(`{"error":{"message":"` + msg + `","type":"` + errType + `","code":"` + code + `"}}`)
	out <- &transport.Message{Headers: headers, Body: body}
}

func sendPoolOutOfCapacityEnvelope(out chan<- *transport.Message) {
	out <- &transport.Message{
		Headers: map[string]string{
			"X-Relay-Status": "429",
			"Content-Type":   "application/json",
			"X-Relay-Final":  "true",
			"Retry-After":    "30",
		},
		Body: []byte(`{"error":{"message":"pool out of capacity: all secrets at zero remaining quota","type":"rate_limit_exceeded","code":"pool_out_of_capacity"}}`),
	}
}

func sendPoolExhausted(out chan<- *transport.Message) {
	out <- &transport.Message{
		Headers: map[string]string{
			"X-Relay-Status": "502",
			"Content-Type":   "application/json",
			"X-Relay-Final":  "true",
		},
		Body: []byte(`{"error":{"message":"all keys in pool exhausted","type":"upstream_error","code":"pool_exhausted"}}`),
	}
}

// send429LimitEnvelope emits an OpenAI-shaped 429 for a relay-side limit violation.
func send429LimitEnvelope(out chan<- *transport.Message, exceeded *limit.ExceededError) {
	code := meterToCode(exceeded.Rule.Meter)
	msg := "rate limit exceeded: " + string(exceeded.Rule.Meter)
	headers := map[string]string{
		"X-Relay-Status": "429",
		"Content-Type":   "application/json",
		"X-Relay-Final":  "true",
	}
	if exceeded.RetryAfter > 0 {
		headers["Retry-After"] = strconv.Itoa(int(exceeded.RetryAfter.Seconds()))
	}
	body := []byte(`{"error":{"message":"` + msg + `","type":"rate_limit_exceeded","code":"` + code + `"}}`)
	out <- &transport.Message{Headers: headers, Body: body}
}

func meterToCode(m configstore.Meter) string {
	switch m {
	case configstore.MeterRequests:
		return "rpm_exceeded"
	case configstore.MeterTokens:
		return "tpm_exceeded"
	case configstore.MeterConcurrency:
		return "concurrency_exceeded"
	default:
		return "rate_limit_exceeded"
	}
}

func sendGenericErrorEnvelope(out chan<- *transport.Message) {
	out <- &transport.Message{
		Headers: map[string]string{
			"X-Relay-Status": "500",
			"Content-Type":   "application/json",
			"X-Relay-Final":  "true",
		},
		Body: []byte(`{"error":{"message":"internal error","type":"internal_error","code":"internal_error"}}`),
	}
}

// peekTokens extracts token usage from a message body and accumulates into *acc.
// Only updates if tokens are found and are greater than the current value.
func peekTokens(b []byte, acc *int64) {
	if len(b) == 0 {
		return
	}
	n, ok := limit.ParseTokens(b)
	if ok && n > *acc {
		*acc = n
	}
}
