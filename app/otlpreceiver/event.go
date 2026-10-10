package otlpreceiver

import (
	"net/http"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/tokencount"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/otlp"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// Extras keys stamped on reported events, beside the instance and session keys proxied traffic already uses.
const (
	ExtrasKeyConvention       = "telemetry_convention"
	ExtrasKeyTraceID          = "trace_id"
	ExtrasKeySpanID           = "span_id"
	ExtrasKeyResponseID       = "response_id"
	ExtrasKeyService          = "service"
	ExtrasKeyOperation        = "operation"
	ExtrasKeyReportedProvider = "reported_provider"
	// ExtrasKeyReportedError holds the error type of a failed call as the client reported it; the event's error kind is relay's own classification of it.
	ExtrasKeyReportedError = "reported_error_type"
	extrasKeyClientIP      = "client_ip"
)

// origin is what the receiving relay knows about an export, as opposed to what the client reported in it.
type origin struct {
	instance string
	clientIP string
}

// buildEvent turns one reported model call into a usage event attributed to reporter. received stands in for a missing start time.
func buildEvent(snap *appcatalog.Snapshot, reporter *lifecycle.Context, c reported, hint ProviderHint, pricer *usagelog.Pricer, from origin, received time.Time) usagelog.Event {
	inf := c.inf
	ev := usagelog.Event{
		RequestID:      c.id.RequestID(),
		Source:         Source,
		Timestamp:      inf.Start,
		Status:         status(inf),
		DurationMs:     inf.Duration.Milliseconds(),
		Streamed:       inf.Streamed,
		FinishReason:   inf.FinishReason,
		ErrorKind:      errorKind(inf),
		RelayKeyHash:   reporter.RelayKeyHash,
		RequestedModel: inf.RequestModel,
		ProjectID:      reporter.ProjectID,
		Project:        reporter.ProjectName,
		TeamID:         reporter.TeamID,
		Team:           reporter.TeamName,
		PrincipalKind:  reporter.PrincipalKind,
		PrincipalID:    reporter.PrincipalID,
		Principal:      reporter.PrincipalName,
		CredentialKind: reporter.CredentialKind,
		CredentialID:   reporter.CredentialID,
		Extras:         extras(inf, from),
	}
	if ev.Timestamp.IsZero() {
		ev.Timestamp = received
	}
	if ev.RequestedModel == "" {
		ev.RequestedModel = inf.ResponseModel
	}
	if inf.TimeToFirstChunk > 0 {
		ev.Upstream = &usagelog.UpstreamTiming{
			ResponseStart: inf.TimeToFirstChunk.Microseconds(),
			// A call reported as an event has no duration; its response cannot end before the first chunk.
			ResponseEnd: max(inf.Duration, inf.TimeToFirstChunk).Microseconds(),
		}
	}

	m := resolveModel(snap, inf, hint)
	ev.ModelID, ev.Model, ev.Provider, ev.Pricing = m.id, m.name, m.provider, m.pricingName
	ev.Tokens = tokens(inf.Tokens)
	if nanos, breakdown, ok := pricer.Price(m.pricingID, ev.Tokens, ""); ok {
		ev.CostNanos = &nanos
		ev.CostBreakdown = breakdown
	}
	return ev
}

// status is the event's HTTP status: the provider's when the telemetry carried it, otherwise 200 for a call that succeeded. A failed call with no status stays 0, which with its error kind makes it a log-only row.
func status(inf otlp.Inference) int {
	switch {
	case inf.HTTPStatus != 0:
		return inf.HTTPStatus
	case inf.ErrorType != "":
		return 0
	}
	return http.StatusOK
}

func tokens(t otlp.TokenCounts) sdkusage.Tokens {
	out := sdkusage.Tokens{}
	for key, n := range map[string]int64{
		"input":          t.Input,
		"output":         t.Output,
		"cache_read":     t.CacheRead,
		"cache_creation": t.CacheWrite,
		"reasoning":      t.Reasoning,
		"audio_input":    t.AudioInput,
		"audio_output":   t.AudioOutput,
	} {
		if n > 0 {
			out[key] = n
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func extras(inf otlp.Inference, from origin) map[string]string {
	out := map[string]string{ExtrasKeyConvention: inf.Convention}
	for key, v := range map[string]string{
		ExtrasKeyTraceID:              inf.TraceID,
		ExtrasKeySpanID:               inf.SpanID,
		ExtrasKeyResponseID:           inf.ResponseID,
		ExtrasKeyService:              inf.Service,
		ExtrasKeyOperation:            inf.Operation,
		ExtrasKeyReportedProvider:     inf.Provider,
		ExtrasKeyReportedError:        inf.ErrorType,
		tokencount.MetadataKeySession: inf.ConversationID,
		usagelog.ExtrasKeyInstance:    from.instance,
		extrasKeyClientIP:             from.clientIP,
	} {
		if v != "" {
			out[key] = v
		}
	}
	return out
}
