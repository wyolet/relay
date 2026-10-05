package otlpreceiver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/otlp"
	"github.com/wyolet/relay/pkg/payload"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Source is the usage event source of every model call recorded from telemetry.
const Source = "otlp"

// TracesPath and LogsPath are where the export endpoints mount. An OTLP exporter appends /v1/<signal> to its configured endpoint, so clients point at <inference url>/otlp.
const (
	TracesPath = "/otlp/v1/traces"
	LogsPath   = "/otlp/v1/logs"
)

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
	boundedQueue
}

// PayloadQueue is the payload pipeline as the receiver needs it, for the message content of reported calls.
type PayloadQueue interface {
	TryEmit(payload.Record) bool
	boundedQueue
}

// PayloadLog is the payload store's live switch and its cap on a stored body in bytes (0 = none).
type PayloadLog interface {
	Enabled() bool
	MaxBytes() int
}

// boundedQueue is a queue the receiver shares with proxied traffic.
type boundedQueue interface {
	Free() int
	Capacity() int
}

// share is how much of a queue reported telemetry may occupy: half, rounded up so a queue of one still takes a record. The other half stays free for proxied traffic, whose records are dropped, not refused, when the queue is full.
func share(q boundedQueue) int { return (q.Capacity() + 1) / 2 }

// sharedRoom is how many more records the receiver may queue before the queue is half full. Other traffic may have filled it past that, which makes it negative.
func sharedRoom(q boundedQueue) int { return share(q) - (q.Capacity() - q.Free()) }

// tenantOf names whose calls a reporter's exports describe: its project, so one service reporting through two keys of a project is recognised as one, or the principal itself when the credential belongs to no project.
func tenantOf(reporter *lifecycle.Context) string {
	if reporter.ProjectID != "" {
		return reporter.ProjectID
	}
	return reporter.PrincipalID
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
	// Limiter enforces the export rate limit per credential. Nil applies none.
	Limiter Limiter
	// Pricer prices each event's tokens. Nil leaves events unpriced.
	Pricer *usagelog.Pricer
	// SpanMappers and LogMappers are the telemetry conventions understood per signal, tried in order.
	SpanMappers []otlp.SpanMapper
	LogMappers  []otlp.LogMapper
	// ProviderHints maps a provider name as telemetry reports it, lowercase, to what it refers to in the catalog.
	ProviderHints map[string]ProviderHint
	// CaptureContent reports whether the receiver is set to store the message content clients report. Read per request. Content is stored only while PayloadLog is enabled as well.
	CaptureContent func() bool
	// PayloadLog and Payloads are the payload store proxied traffic uses. Without both, content is never stored.
	PayloadLog PayloadLog
	Payloads   PayloadQueue
	// InstanceID is stamped on events the same way as on proxied traffic.
	InstanceID string
	// MaxBodyBytes bounds a decompressed export body.
	MaxBodyBytes int64
}

// Handler serves the OTLP/HTTP export endpoints. It expects the inference authentication middleware in front of it.
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

// Traces serves POST TracesPath.
func (h *Handler) Traces() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.serve(w, r, traces) })
}

// Logs serves POST LogsPath.
func (h *Handler) Logs() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.serve(w, r, logs) })
}

// serve handles one export of sig. Everything past decoding is the same for every signal.
func (h *Handler) serve(w http.ResponseWriter, r *http.Request, sig signal) {
	if h.opts.Enabled == nil || !h.opts.Enabled() {
		sig.result(resultDisabled).Inc()
		writeStatus(w, otlp.MediaTypeJSON, http.StatusNotFound, otlp.StatusNotFound, "the OTLP receiver is disabled")
		return
	}
	mediaType, err := otlp.MediaType(r.Header.Get("Content-Type"))
	if err != nil {
		sig.result(resultUnsupported).Inc()
		writeStatus(w, otlp.MediaTypeJSON, http.StatusUnsupportedMediaType, otlp.StatusInvalidArgument, "Content-Type must be application/x-protobuf or application/json")
		return
	}

	ctx := r.Context()
	snap := inference.SnapshotFrom(ctx)
	if snap == nil && h.opts.Snapshot != nil {
		snap = h.opts.Snapshot()
	}
	// One identity lookup per export: every record in it is reported by the same caller.
	reporter := lifecycle.NewContext("", Source, time.Time{})
	inference.StampPrincipal(ctx, reporter)

	// Checked before the body is read, so a client over its limit costs one kv call and no decoding.
	if wait, limited := h.rateLimited(ctx, snap, reporter); limited {
		sig.result(resultRateLimited).Inc()
		setRetryAfter(w, wait)
		writeStatus(w, mediaType, http.StatusTooManyRequests, otlp.StatusResourceExhausted, "export rate limit exceeded")
		return
	}

	body, err := otlp.ReadBody(r.Body, r.Header.Get("Content-Encoding"), h.opts.MaxBodyBytes)
	if err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.Is(err, otlp.ErrBodyTooLarge), errors.As(err, &tooLarge):
			sig.result(resultTooLarge).Inc()
			writeStatus(w, mediaType, http.StatusRequestEntityTooLarge, otlp.StatusInvalidArgument, "export body too large")
		case errors.Is(err, otlp.ErrUnsupportedEncoding):
			sig.result(resultUnsupported).Inc()
			writeStatus(w, mediaType, http.StatusUnsupportedMediaType, otlp.StatusInvalidArgument, "Content-Encoding must be gzip or identity")
		default:
			sig.result(resultBadRequest).Inc()
			writeStatus(w, mediaType, http.StatusBadRequest, otlp.StatusInvalidArgument, "could not read the export body")
		}
		return
	}
	calls, total, err := sig.decode(h, mediaType, body)
	if err != nil {
		sig.result(resultBadRequest).Inc()
		writeStatus(w, mediaType, http.StatusBadRequest, otlp.StatusInvalidArgument, "could not decode the export body")
		return
	}
	if len(calls) > MaxRecords {
		sig.result(resultTooManyRecords).Inc()
		writeStatus(w, mediaType, http.StatusBadRequest, otlp.StatusInvalidArgument, fmt.Sprintf("export reports %d model calls; the limit is %d per export", len(calls), MaxRecords))
		return
	}

	now := time.Now()
	accepted, refused := admit(calls, sig.responseIdentity, now)
	if len(accepted) > 0 {
		from := origin{instance: h.opts.InstanceID, clientIP: inference.ClassificationFrom(ctx).ClientIP}
		if !h.record(ctx, sig, snap, reporter, from, accepted, now) {
			sig.result(resultBackpressure).Inc()
			setRetryAfter(w, queueRetryAfter)
			writeStatus(w, mediaType, http.StatusServiceUnavailable, otlp.StatusUnavailable, "usage queue is full; send the export again")
			return
		}
	}

	sig.records.WithLabelValues(outcomeIgnored).Add(float64(total - len(calls)))
	sig.records.WithLabelValues(outcomeDuplicate).Add(float64(refused.repeated))
	sig.records.WithLabelValues(outcomeRejected).Add(float64(refused.rejected()))
	sig.result(resultAccepted).Inc()
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(otlp.ExportResponse(mediaType, sig.wire, refused.rejected(), refused.message(sig)))
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
