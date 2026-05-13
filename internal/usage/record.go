package usage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"runtime/debug"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/wyolet/relay/pkg/eventlog"
)

// attemptsToRecords converts []Attempt to []eventlog.AttemptRecord.
func attemptsToRecords(as []Attempt) []eventlog.AttemptRecord {
	if as == nil {
		return nil
	}
	out := make([]eventlog.AttemptRecord, len(as))
	for i, a := range as {
		out[i] = eventlog.AttemptRecord{
			SecretHash: a.SecretHash,
			Outcome:    a.Outcome,
			HTTPStatus: a.HTTPStatus,
			LatencyMS:  a.LatencyMS,
		}
	}
	return out
}

var (
	cachedInstanceID   string
	cachedRelayVersion string
	defaultEventLogger *eventlog.Logger
)

// InstanceID returns the resolved instance ID set during Init.
func InstanceID() string { return cachedInstanceID }

// RelayVersion returns the version string from Go build info, or "dev".
func RelayVersion() string { return cachedRelayVersion }

func init() {
	cachedRelayVersion = resolveRelayVersion()
	// InstanceID is populated by Init; fall back to hostname until then.
	cachedInstanceID = resolveInstanceIDFallback("")
}

// resolveInstanceIDFallback resolves instance ID: use hint if non-empty,
// else hostname, else "unknown". Does NOT read env vars.
func resolveInstanceIDFallback(hint string) string {
	if hint != "" {
		return hint
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

func resolveRelayVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// SecretHash returns the first 12 hex chars of the sha256 of raw.
func SecretHash(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])[:12]
}

// Record serializes lc into a JSON event, hands it to the eventlog, and ends the OTel span.
// Never returns an error; internal failures increment counters and WARN-log only.
func Record(ctx context.Context, lc *Lifecycle) {
	instanceID := lc.InstanceID
	if instanceID == "" {
		instanceID = cachedInstanceID
	}
	relayVersion := lc.RelayVersion
	if relayVersion == "" {
		relayVersion = cachedRelayVersion
	}

	// Cost computation: removed alongside the legacy catalog.Pricing
	// type in stage 5. A new-arch pricing lookup against app/pricing
	// will be rewired when the observability emit pipeline is rebuilt.

	rec := eventlog.Event{
		EventVersion: 1,
		RequestID:    lc.RequestID,
		Model:        lc.Model,
		Provider:     lc.Provider,
		Policy:         lc.Policy,
		SecretHash:   lc.SecretHash,
		TerminatedBy: string(lc.TerminatedBy),
		Tokens:       map[string]int64(lc.Tokens),
		Attempts:     attemptsToRecords(lc.Attempts),
		Attribution:  lc.Attribution,
		Metrics:      lc.Metrics,
		InstanceID:   instanceID,
		RelayVersion: relayVersion,
		StartedAt:    lc.StartedAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
		EndedAt:      lc.EndedAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
		Cost:         lc.Cost,
		Currency:     lc.Currency,
	}

	if defaultEventLogger != nil {
		if err := defaultEventLogger.Append(ctx, rec); err != nil {
			metricDroppedEvents.Inc()
			slog.WarnContext(ctx, "usage.Record: eventlog drop", "err", err)
		}
	}

	// Increment per-type token counter. One observation per (provider, type).
	if len(lc.Tokens) > 0 {
		provider := lc.Provider
		for tokenType, count := range lc.Tokens {
			if count > 0 {
				metricTokensTotal.WithLabelValues(provider, tokenType).Add(float64(count))
			}
		}
	}

	sp := lc.Span()
	if sp == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("relay.request_id", lc.RequestID),
		attribute.String("relay.model", lc.Model),
		attribute.String("relay.provider", lc.Provider),
		attribute.String("relay.policy", lc.Policy),
		attribute.String("relay.secret_hash", lc.SecretHash),
		attribute.String("relay.terminated_by", string(lc.TerminatedBy)),
		attribute.Int("relay.event_version", 1),
		attribute.String("relay.instance_id", instanceID),
		attribute.String("relay.relay_version", relayVersion),
	}
	for k, v := range lc.Tokens {
		attrs = append(attrs, attribute.Int64("relay.tokens."+k, v))
	}
	if lc.Cost > 0 {
		attrs = append(attrs,
			attribute.Float64("relay.cost", lc.Cost),
			attribute.String("relay.currency", lc.Currency),
		)
	}
	for k, v := range lc.Metrics {
		attrs = append(attrs, attribute.Int64("relay.metrics."+k, v))
	}
	for k, v := range lc.Attribution {
		attrs = append(attrs, attribute.String("relay.attr."+k, v))
	}
	if len(lc.Attempts) > 0 {
		b, err := json.Marshal(lc.Attempts)
		if err == nil {
			attrs = append(attrs, attribute.String("relay.attempts", string(b)))
		}
	}
	sp.SetAttributes(attrs...)

	if lc.TerminatedBy == TerminatedClean {
		sp.SetStatus(codes.Ok, "")
	} else {
		sp.SetStatus(codes.Error, string(lc.TerminatedBy))
	}
	sp.End()
}
