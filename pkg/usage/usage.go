package usage

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/wyolet/relay/pkg/eventlog"
)

const (
	SpanName    = "relay.request"
	AttemptsCap = 10
)

// TerminatedBy describes how a request ended.
type TerminatedBy string

const (
	TerminatedClean           TerminatedBy = "clean"
	TerminatedClientCancel    TerminatedBy = "client_cancel"
	TerminatedUpstreamError   TerminatedBy = "upstream_error"
	TerminatedUpstreamTimeout TerminatedBy = "upstream_timeout"
	TerminatedRateLimited     TerminatedBy = "rate_limited"
	TerminatedPoolExhausted   TerminatedBy = "pool_exhausted"
	TerminatedRelayError      TerminatedBy = "relay_error"
)

// TokenBlock holds token counts from the provider response.
type TokenBlock struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Total      int64 `json:"total"`
	Cached     int64 `json:"cached,omitempty"`
}

// Attempt records a single upstream call within a request.
type Attempt struct {
	SecretHash string `json:"secret_hash"`
	Outcome    string `json:"outcome"` // success | http_4xx | http_5xx | network_error | rate_limited
	HTTPStatus int    `json:"http_status,omitempty"`
	LatencyMS  int64  `json:"latency_ms"`
}

// Lifecycle is the per-request record assembled by the pipeline.
// The span field is unexported; pipeline code accesses it via Span().
type Lifecycle struct {
	RequestID     string            `json:"request_id"`
	Model         string            `json:"model"`
	Provider      string            `json:"provider"`
	Pool          string            `json:"pool"`
	SecretHash    string            `json:"secret_hash"`
	Attempts      []Attempt         `json:"attempts,omitempty"`
	Tokens        TokenBlock        `json:"tokens"`
	TerminatedBy  TerminatedBy      `json:"terminated_by"`
	Attribution   map[string]string `json:"attribution,omitempty"`
	Metrics       map[string]int64  `json:"metrics,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	EndedAt       time.Time         `json:"ended_at"`
	InstanceID    string            `json:"instance_id"`
	RelayVersion  string            `json:"relay_version"`

	span trace.Span
}

// Span returns the OTel span associated with this lifecycle. May be a no-op span.
func (l *Lifecycle) Span() trace.Span { return l.span }

// SetSpan stores the OTel span. Called by the pipeline after starting the span.
func (l *Lifecycle) SetSpan(s trace.Span) { l.span = s }

// ctxSpanKey is the context key for the OTel span started in reqid middleware.
type ctxSpanKey struct{}

// ContextWithSpan returns a new context carrying the given span.
func ContextWithSpan(ctx context.Context, sp trace.Span) context.Context {
	return context.WithValue(ctx, ctxSpanKey{}, sp)
}

// SpanFromContext retrieves the span stored by ContextWithSpan. Returns nil if absent.
func SpanFromContext(ctx context.Context) trace.Span {
	sp, _ := ctx.Value(ctxSpanKey{}).(trace.Span)
	return sp
}

// Config controls TracerProvider initialization.
type Config struct {
	// OTLPEndpoint is the host:port of the OTLP/gRPC collector.
	// Empty → no-op TracerProvider.
	OTLPEndpoint string

	// ServiceName is used in the OTel resource. Defaults to "relay".
	ServiceName string

	// BatchQueueSize overrides the default OTel batch processor queue size (2048).
	BatchQueueSize int

	// EventLog, when non-nil, receives serialized events from Record.
	// When nil, Record skips the eventlog write but still ends the OTel span.
	EventLog *eventlog.Logger
}

// Shutdown is a function that tears down the TracerProvider.
type Shutdown func(context.Context) error

// droppedSpans counts spans dropped by the custom counting exporter shim.
// TODO(PER-239): wire to Prometheus gauge.
// Triggered when the batch processor queue is full and the exporter's Export
// call returns an error that the shim treats as a drop signal.
var droppedSpans atomic.Uint64

// DroppedSpans returns the cumulative count of dropped spans since process start.
func DroppedSpans() uint64 { return droppedSpans.Load() }

// countingExporter wraps an SpanExporter and increments droppedSpans when
// the underlying exporter returns an error (batch overflow manifests here).
type countingExporter struct {
	sdktrace.SpanExporter
}

func (c *countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := c.SpanExporter.ExportSpans(ctx, spans)
	if err != nil {
		droppedSpans.Add(uint64(len(spans)))
	}
	return err
}

// Init installs a TracerProvider as the global OTel provider.
// When cfg.OTLPEndpoint is empty a no-op provider is used.
// Returns a Shutdown function that is safe to call multiple times.
func Init(ctx context.Context, cfg Config) (Shutdown, error) {
	if cfg.OTLPEndpoint == "" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		defaultEventLogger = cfg.EventLog
		return func(context.Context) error { return nil }, nil
	}

	if cfg.ServiceName == "" {
		cfg.ServiceName = "relay"
	}
	if cfg.BatchQueueSize <= 0 {
		cfg.BatchQueueSize = 2048
	}

	exp, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithInsecure(),
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
	)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(cfg.ServiceName)),
	)
	if err != nil {
		return nil, err
	}

	bsp := sdktrace.NewBatchSpanProcessor(
		&countingExporter{exp},
		sdktrace.WithMaxQueueSize(cfg.BatchQueueSize),
	)

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)

	otel.SetTracerProvider(tp)
	defaultEventLogger = cfg.EventLog

	var once atomic.Bool
	return func(ctx context.Context) error {
		if !once.CompareAndSwap(false, true) {
			return nil
		}
		return tp.Shutdown(ctx)
	}, nil
}
