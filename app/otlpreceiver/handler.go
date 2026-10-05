package otlpreceiver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/otlp"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Source is the usage event source of every model call recorded from telemetry.
const Source = "otlp"

// TracesPath is where the trace export endpoint mounts. An OTLP exporter appends /v1/traces to its configured endpoint, so clients point at <inference url>/otlp.
const TracesPath = "/otlp/v1/traces"

// DefaultMaxBodyBytes bounds a decompressed export when Options.MaxBodyBytes is unset.
const DefaultMaxBodyBytes = 16 << 20

// MaxRecords is the most model calls one export may report.
const MaxRecords = 10_000

// maxClockSkew is how far ahead of the receiver's clock a model call may start. Anything later is a broken client clock, and a row dated in the future would sit outside every query window until then.
const maxClockSkew = 5 * time.Minute

// queueRetryAfter is the delay asked of a client refused for lack of room in the usage queue, which drains in well under a second once its sink accepts writes.
const queueRetryAfter = time.Second

// UsageQueue is the usage pipeline as the receiver needs it: a bounded queue that reports a full queue instead of dropping, so the export can be refused and sent again.
type UsageQueue interface {
	TryEmit(usagelog.Event) bool
	Free() int
	Capacity() int
}

// Limiter is the rate limiter the receiver reserves one request on per export.
type Limiter interface {
	Reserve(ctx context.Context, scope string, rules []pkgratelimit.Rule) (*pkgratelimit.Reservation, error)
}

// Options configures a Handler.
type Options struct {
	// Enabled reports whether the receiver currently accepts exports. Read per request so the setting applies without a restart.
	Enabled func() bool
	// Snapshot returns the current catalog snapshot, used when the request context carries none.
	Snapshot func() *appcatalog.Snapshot
	// Usage takes the recorded events.
	Usage UsageQueue
	// Markers recognises calls that were already recorded. Nil records every call it is sent.
	Markers *Markers
	// Limiter enforces the RateLimitName system rate limit per credential. Nil applies none.
	Limiter Limiter
	// Pricer prices each event's tokens. Nil leaves events unpriced.
	Pricer *usagelog.Pricer
	// Mappers are the telemetry conventions understood, tried in order.
	Mappers []otlp.SpanMapper
	// ProviderHints maps a provider name as telemetry reports it, lowercase, to what it refers to in the catalog.
	ProviderHints map[string]ProviderHint
	// InstanceID is stamped on events the same way as on proxied traffic.
	InstanceID string
	// MaxBodyBytes bounds a decompressed export body.
	MaxBodyBytes int64
}

// Handler serves POST /otlp/v1/traces. It expects the inference authentication middleware in front of it.
type Handler struct {
	opts Options
}

// New returns a Handler.
func New(o Options) *Handler {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = DefaultMaxBodyBytes
	}
	return &Handler{opts: o}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.opts.Enabled == nil || !h.opts.Enabled() {
		exportsTotal.WithLabelValues(resultDisabled).Inc()
		writeStatus(w, otlp.MediaTypeJSON, http.StatusNotFound, otlp.StatusNotFound, "the OTLP receiver is disabled")
		return
	}
	mediaType, err := otlp.MediaType(r.Header.Get("Content-Type"))
	if err != nil {
		exportsTotal.WithLabelValues(resultUnsupported).Inc()
		writeStatus(w, otlp.MediaTypeJSON, http.StatusUnsupportedMediaType, otlp.StatusInvalidArgument, "Content-Type must be application/x-protobuf or application/json")
		return
	}

	ctx := r.Context()
	snap := inference.SnapshotFrom(ctx)
	if snap == nil && h.opts.Snapshot != nil {
		snap = h.opts.Snapshot()
	}
	// One identity lookup per export: every span in it is reported by the same caller.
	reporter := lifecycle.NewContext("", Source, time.Time{})
	inference.StampPrincipal(ctx, reporter)

	// Checked before the body is read, so a client over its limit costs one kv call and no decoding.
	if wait, limited := h.rateLimited(ctx, snap, reporter); limited {
		exportsTotal.WithLabelValues(resultRateLimited).Inc()
		setRetryAfter(w, wait)
		writeStatus(w, mediaType, http.StatusTooManyRequests, otlp.StatusResourceExhausted, "export rate limit exceeded")
		return
	}

	body, err := otlp.ReadBody(r.Body, r.Header.Get("Content-Encoding"), h.opts.MaxBodyBytes)
	if err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.Is(err, otlp.ErrBodyTooLarge), errors.As(err, &tooLarge):
			exportsTotal.WithLabelValues(resultTooLarge).Inc()
			writeStatus(w, mediaType, http.StatusRequestEntityTooLarge, otlp.StatusInvalidArgument, "export body too large")
		case errors.Is(err, otlp.ErrUnsupportedEncoding):
			exportsTotal.WithLabelValues(resultUnsupported).Inc()
			writeStatus(w, mediaType, http.StatusUnsupportedMediaType, otlp.StatusInvalidArgument, "Content-Encoding must be gzip or identity")
		default:
			exportsTotal.WithLabelValues(resultBadRequest).Inc()
			writeStatus(w, mediaType, http.StatusBadRequest, otlp.StatusInvalidArgument, "could not read the export body")
		}
		return
	}
	spans, err := otlp.DecodeTraces(mediaType, body)
	if err != nil {
		exportsTotal.WithLabelValues(resultBadRequest).Inc()
		writeStatus(w, mediaType, http.StatusBadRequest, otlp.StatusInvalidArgument, "could not decode the export body")
		return
	}

	var calls []otlp.Inference
	for _, s := range spans {
		if inf, ok := h.mapSpan(s); ok {
			calls = append(calls, inf)
		}
	}
	if len(calls) > MaxRecords {
		exportsTotal.WithLabelValues(resultTooManyRecords).Inc()
		writeStatus(w, mediaType, http.StatusBadRequest, otlp.StatusInvalidArgument, fmt.Sprintf("export reports %d model calls; the limit is %d per export", len(calls), MaxRecords))
		return
	}

	now := time.Now()
	accepted, refused := admit(calls, now)
	if len(accepted) > 0 {
		from := origin{instance: h.opts.InstanceID, clientIP: inference.ClassificationFrom(ctx).ClientIP}
		if !h.record(ctx, snap, reporter, from, accepted, now) {
			exportsTotal.WithLabelValues(resultBackpressure).Inc()
			setRetryAfter(w, queueRetryAfter)
			writeStatus(w, mediaType, http.StatusServiceUnavailable, otlp.StatusUnavailable, "usage queue is full; send the export again")
			return
		}
	}

	spansTotal.WithLabelValues(outcomeIgnored).Add(float64(len(spans) - len(calls)))
	spansTotal.WithLabelValues(outcomeDuplicate).Add(float64(refused.repeated))
	spansTotal.WithLabelValues(outcomeRejected).Add(float64(refused.rejected()))
	exportsTotal.WithLabelValues(resultAccepted).Inc()
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(otlp.TraceResponse(mediaType, refused.rejected(), refused.message()))
}

func (h *Handler) mapSpan(s otlp.Span) (otlp.Inference, bool) {
	for _, m := range h.opts.Mappers {
		if inf, ok := m.MapSpan(s); ok {
			return inf, true
		}
	}
	return otlp.Inference{}, false
}

// refusals counts the model calls of one export that are not recorded, by reason.
type refusals struct {
	noIdentity int64
	future     int64
	// repeated counts calls that appear more than once in the export. They are duplicates, not rejections.
	repeated int64
}

func (r refusals) rejected() int64 { return r.noIdentity + r.future }

func (r refusals) message() string {
	var parts []string
	if r.noIdentity > 0 {
		parts = append(parts, strconv.FormatInt(r.noIdentity, 10)+" without a trace id or span id")
	}
	if r.future > 0 {
		parts = append(parts, fmt.Sprintf("%d starting more than %d minutes in the future", r.future, int(maxClockSkew/time.Minute)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "model-call spans rejected: " + strings.Join(parts, "; ")
}

// admit returns the calls that may be recorded, each once, and counts the rest.
func admit(calls []otlp.Inference, now time.Time) ([]otlp.Inference, refusals) {
	var refused refusals
	latestStart := now.Add(maxClockSkew)
	inExport := make(map[Call]struct{}, len(calls))
	accepted := calls[:0]
	for _, inf := range calls {
		id := Call{TraceID: inf.TraceID, SpanID: inf.SpanID}
		switch {
		// A call's identity is its span's ids; without them a resent span could not be told from a new one.
		case inf.TraceID == "" || inf.SpanID == "":
			refused.noIdentity++
			continue
		case inf.Start.After(latestStart):
			refused.future++
			continue
		}
		if _, ok := inExport[id]; ok {
			refused.repeated++
			continue
		}
		inExport[id] = struct{}{}
		accepted = append(accepted, inf)
	}
	return accepted, refused
}

// record queues one usage event per call not recorded before. It reports false when the usage queue could not take them all: the client must then send the export again, and whatever was queued this time is skipped as a duplicate.
func (h *Handler) record(ctx context.Context, snap *appcatalog.Snapshot, reporter *lifecycle.Context, from origin, calls []otlp.Inference, now time.Time) bool {
	queue := h.opts.Usage
	// Refused before anything is marked when the export cannot fit. An export larger than the whole queue never fits, so it is taken in parts across resends instead.
	if room := queue.Free(); room == 0 || (room < len(calls) && len(calls) <= queue.Capacity()) {
		return false
	}

	ids := make([]Call, len(calls))
	for i, inf := range calls {
		ids[i] = Call{TraceID: inf.TraceID, SpanID: inf.SpanID}
	}
	fresh := h.markUsage(ctx, ids)

	var unqueued []Call
	for i, inf := range calls {
		if !fresh[i] {
			spansTotal.WithLabelValues(outcomeDuplicate).Inc()
			continue
		}
		hint := h.opts.ProviderHints[strings.ToLower(strings.TrimSpace(inf.Provider))]
		if len(unqueued) > 0 || !queue.TryEmit(buildEvent(snap, reporter, inf, hint, h.opts.Pricer, from, now)) {
			unqueued = append(unqueued, ids[i])
			continue
		}
		spansTotal.WithLabelValues(outcomeRecorded).Inc()
	}
	if len(unqueued) == 0 {
		return true
	}
	if h.opts.Markers != nil {
		// Detached from the request: a client that hangs up must not leave calls marked as recorded that were never queued.
		if err := h.opts.Markers.Unmark(context.WithoutCancel(ctx), MarkerUsage, unqueued); err != nil {
			markerErrors.WithLabelValues(opUnmark).Inc()
			slog.Default().Warn("otlp receiver: calls stay marked as recorded but were not queued", "calls", len(unqueued), "err", err)
		}
	}
	return false
}

// markUsage reports which calls have not been recorded before. A failing store answers "none were": recording a call twice is the smaller harm than losing it.
func (h *Handler) markUsage(ctx context.Context, ids []Call) []bool {
	if h.opts.Markers != nil {
		fresh, err := h.opts.Markers.Mark(ctx, MarkerUsage, ids)
		if err != nil {
			markerErrors.WithLabelValues(opMark).Inc()
			slog.Default().Warn("otlp receiver: duplicate check failed; recording without it", "err", err)
		}
		return fresh
	}
	fresh := make([]bool, len(ids))
	for i := range fresh {
		fresh[i] = true
	}
	return fresh
}

func writeStatus(w http.ResponseWriter, mediaType string, httpStatus int, code int32, message string) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(httpStatus)
	_, _ = w.Write(otlp.StatusBody(mediaType, code, message))
}

// setRetryAfter writes the delay in whole seconds, rounded up and at least 1: exporters read 0 as "retry now".
func setRetryAfter(w http.ResponseWriter, d time.Duration) {
	w.Header().Set("Retry-After", strconv.FormatInt(max(1, int64((d+time.Second-1)/time.Second)), 10))
}
