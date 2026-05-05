package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/wyolet/relay/pkg/configstore"
	"github.com/wyolet/relay/pkg/keypool"
	"github.com/wyolet/relay/pkg/provider"
	"github.com/wyolet/relay/pkg/transport"
)

var (
	ErrNoInboundMessage = errors.New("pipeline: no inbound message on Channel.In")
	ErrAttemptsExhausted = errors.New("pipeline: all attempts exhausted")
)

const defaultMaxAttempts = 3
const shortRateLimitThreshold = 5 * time.Second

// RunOptions configures a Run invocation.
type RunOptions struct {
	Pool        *configstore.Pool
	Secrets     []*configstore.Secret
	Selector    *keypool.Selector
	Outbound    provider.Outbound
	MaxAttempts int // 0 → 3
}

// Run reads the inbound Message from ch.In and orchestrates upstream calls
// with retry/failover. It closes ch.Out before returning. Pre-first-byte
// retry only: once a non-error first response chunk is forwarded, the
// response is committed.
func Run(ctx context.Context, ch *transport.Channel, opts RunOptions) error {
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}

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

	defer close(ch.Out)

	attempt := 0
	sameKeyAttempt := 0
	var chosenKey *configstore.Secret
	lastFailureKind := keypool.FailureKind(-1)
	var maxRetryAfter time.Duration

	for attempt < maxAttempts {
		attempt++

		// Pick a key if we don't have one.
		if chosenKey == nil {
			var err error
			chosenKey, err = opts.Selector.Pick(ctx, opts.Pool, opts.Secrets)
			if err != nil {
				if errors.Is(err, keypool.ErrNoHealthyKeys) {
					sendExhaustedEnvelope(ch.Out, keypool.FailureKind(-2), 0)
					return err
				}
				return err
			}
			sameKeyAttempt = 0
		}

		secret := chosenKey

		// Spawn outbound into intermediate channel.
		inter := make(chan *transport.Message, 64)
		outboundErr := make(chan error, 1)
		go func() {
			outboundErr <- opts.Outbound.ChatCompletions(ctx, inboundMsg.Body, secret.Resolved, inter)
		}()

		// Read first message.
		var firstMsg *transport.Message
		select {
		case msg, ok := <-inter:
			if !ok || msg == nil {
				// Channel closed without message — treat as network failure.
				<-outboundErr
				opts.Selector.RecordFailure(ctx, secret.KeyHash, keypool.FailureNetwork, 0)
				lastFailureKind = keypool.FailureNetwork
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
			ch.Out <- firstMsg
			for msg := range inter {
				select {
				case <-ctx.Done():
					go drain(inter)
					<-outboundErr
					return ctx.Err()
				default:
				}
				ch.Out <- msg
			}
			<-outboundErr
			return nil

		case classAuth:
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
			chosenKey = nil
			sameKeyAttempt = 0

		case classRateLimit429Short:
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
			chosenKey = nil
			sameKeyAttempt = 0

		case classServerError, classNetwork:
			go drain(inter)
			<-outboundErr
			kind := keypool.FailureServerError
			classification := "5xx"
			if cls == classNetwork {
				kind = keypool.FailureNetwork
				classification = "network"
			}
			opts.Selector.RecordFailure(ctx, secret.KeyHash, kind, 0)
			lastFailureKind = kind
			slog.Default().Info("pipeline attempt",
				"attempt", attempt,
				"key_hash", secret.KeyHash,
				"status", status,
				"classification", classification,
			)
			if sameKeyAttempt == 0 {
				sameKeyAttempt++
			} else {
				chosenKey = nil
				sameKeyAttempt = 0
			}
		}
	}

	sendExhaustedEnvelope(ch.Out, lastFailureKind, maxRetryAfter)
	return ErrAttemptsExhausted
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
