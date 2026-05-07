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

// InstanceID returns the resolved instance ID (RELAY_INSTANCE_ID → hostname → "unknown").
func InstanceID() string { return cachedInstanceID }

// RelayVersion returns the version string from Go build info, or "dev".
func RelayVersion() string { return cachedRelayVersion }

func init() {
	cachedInstanceID = resolveInstanceID()
	cachedRelayVersion = resolveRelayVersion()
}

func resolveInstanceID() string {
	if v := os.Getenv("RELAY_INSTANCE_ID"); v != "" {
		return v
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

	rec := eventlog.Event{
		EventVersion: 1,
		RequestID:    lc.RequestID,
		Model:        lc.Model,
		Provider:     lc.Provider,
		Pool:         lc.Pool,
		SecretHash:   lc.SecretHash,
		TerminatedBy: string(lc.TerminatedBy),
		Tokens: eventlog.TokenCounts{
			Prompt:     lc.Tokens.Prompt,
			Completion: lc.Tokens.Completion,
			Total:      lc.Tokens.Total,
			Cached:     lc.Tokens.Cached,
		},
		Attempts:     attemptsToRecords(lc.Attempts),
		Attribution:  lc.Attribution,
		Metrics:      lc.Metrics,
		InstanceID:   instanceID,
		RelayVersion: relayVersion,
		StartedAt:    lc.StartedAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
		EndedAt:      lc.EndedAt.UTC().Format("2006-01-02T15:04:05.999999999Z"),
	}

	if defaultEventLogger != nil {
		if err := defaultEventLogger.Append(ctx, rec); err != nil {
			metricDroppedEvents.Inc()
			slog.WarnContext(ctx, "usage.Record: eventlog drop", "err", err)
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
		attribute.String("relay.pool", lc.Pool),
		attribute.String("relay.secret_hash", lc.SecretHash),
		attribute.String("relay.terminated_by", string(lc.TerminatedBy)),
		attribute.Int("relay.event_version", 1),
		attribute.String("relay.instance_id", instanceID),
		attribute.String("relay.relay_version", relayVersion),
		attribute.Int64("relay.tokens.prompt", lc.Tokens.Prompt),
		attribute.Int64("relay.tokens.completion", lc.Tokens.Completion),
		attribute.Int64("relay.tokens.total", lc.Tokens.Total),
	}
	if lc.Tokens.Cached != 0 {
		attrs = append(attrs, attribute.Int64("relay.tokens.cached", lc.Tokens.Cached))
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
