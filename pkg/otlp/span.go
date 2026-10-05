package otlp

import "time"

// Span is one exported span with its resource and scope flattened onto it, so a mapper reads everything about the span from one value.
type Span struct {
	// TraceID, SpanID and ParentSpanID are lowercase hex; ParentSpanID is empty on a root span.
	TraceID      string
	SpanID       string
	ParentSpanID string

	Name  string
	Start time.Time
	End   time.Time

	// Failed reports a span status of ERROR.
	Failed        bool
	StatusMessage string

	Attrs    Attrs
	Resource Attrs
	// Scope is the instrumentation scope name.
	Scope string
}

// Inference is one model call as a client's telemetry reported it. Fields the telemetry did not carry stay zero.
type Inference struct {
	// Convention is the Name of the SpanMapper that produced the record.
	Convention string

	TraceID string
	SpanID  string
	// Service is the reporting application's service.name resource attribute.
	Service string

	Start    time.Time
	Duration time.Duration

	Operation     string
	Provider      string
	RequestModel  string
	ResponseModel string
	// ResponseID is the provider's identifier for the response.
	ResponseID     string
	ConversationID string
	FinishReason   string

	Streamed         bool
	TimeToFirstChunk time.Duration

	// ErrorType is set when the call failed; HTTPStatus is the provider's response status when the telemetry carried one.
	ErrorType  string
	HTTPStatus int

	Tokens TokenCounts
}

// TokenCounts are the token counts of one model call. Input excludes cached tokens, which CacheRead and CacheWrite count. Reasoning and AudioOutput are parts of Output, and AudioInput is a part of the input, not additions to them.
type TokenCounts struct {
	Input       int64
	Output      int64
	CacheRead   int64
	CacheWrite  int64
	Reasoning   int64
	AudioInput  int64
	AudioOutput int64
}

// SpanMapper recognises the spans of one telemetry convention that describe a model call.
type SpanMapper interface {
	// Name identifies the convention.
	Name() string
	// MapSpan reports false for a span that is not a model call under this convention. Such a span is left for the next mapper.
	MapSpan(s Span) (Inference, bool)
}
