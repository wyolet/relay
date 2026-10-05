package otlpreceiver

import (
	"errors"
	"net/http"
	"time"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/otlp"
)

// Source is the usage event source of every model call recorded from telemetry.
const Source = "otlp"

// TracesPath is where the trace export endpoint mounts. An OTLP exporter appends /v1/traces to its configured endpoint, so clients point at <inference url>/otlp.
const TracesPath = "/otlp/v1/traces"

// DefaultMaxBodyBytes bounds a decompressed export when Options.MaxBodyBytes is unset.
const DefaultMaxBodyBytes = 16 << 20

// Options configures a Handler.
type Options struct {
	// Enabled reports whether the receiver currently accepts exports. Read per request so the setting applies without a restart.
	Enabled func() bool
	// Snapshot returns the current catalog snapshot, used when the request context carries none.
	Snapshot func() *appcatalog.Snapshot
	// Emit hands a recorded event to the usage pipeline. It must not block.
	Emit func(usagelog.Event)
	// Pricer prices each event's tokens. Nil leaves events unpriced.
	Pricer *usagelog.Pricer
	// Mappers are the telemetry conventions understood, tried in order.
	Mappers []otlp.SpanMapper
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

	ctx := r.Context()
	snap := inference.SnapshotFrom(ctx)
	if snap == nil && h.opts.Snapshot != nil {
		snap = h.opts.Snapshot()
	}
	// One identity lookup per export: every span in it is reported by the same caller.
	reporter := lifecycle.NewContext("", Source, time.Time{})
	inference.StampPrincipal(ctx, reporter)
	origin := origin{instance: h.opts.InstanceID, clientIP: inference.ClassificationFrom(ctx).ClientIP}

	var rejected int64
	now := time.Now()
	for _, s := range spans {
		inf, ok := h.mapSpan(s)
		if !ok {
			spansTotal.WithLabelValues(outcomeIgnored).Inc()
			continue
		}
		// The event id derives from the span's ids; without them a resent span could not be told from a new one.
		if inf.TraceID == "" || inf.SpanID == "" {
			spansTotal.WithLabelValues(outcomeRejected).Inc()
			rejected++
			continue
		}
		h.opts.Emit(buildEvent(snap, reporter, inf, h.opts.Pricer, origin, now))
		spansTotal.WithLabelValues(outcomeRecorded).Inc()
	}

	exportsTotal.WithLabelValues(resultAccepted).Inc()
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(otlp.TraceResponse(mediaType, rejected, "model-call span without a trace id or span id"))
}

func (h *Handler) mapSpan(s otlp.Span) (otlp.Inference, bool) {
	for _, m := range h.opts.Mappers {
		if inf, ok := m.MapSpan(s); ok {
			return inf, true
		}
	}
	return otlp.Inference{}, false
}

func writeStatus(w http.ResponseWriter, mediaType string, httpStatus int, code int32, message string) {
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(httpStatus)
	_, _ = w.Write(otlp.StatusBody(mediaType, code, message))
}
