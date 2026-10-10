package telemetry

import (
	"time"

	"github.com/wyolet/relay/sdk/usage"
)

// Attribute names, current GenAI semantic conventions.
const (
	attrOperation     = "gen_ai.operation.name"
	attrProvider      = "gen_ai.provider.name"
	attrRequestModel  = "gen_ai.request.model"
	attrStream        = "gen_ai.request.stream"
	attrResponseModel = "gen_ai.response.model"
	attrResponseID    = "gen_ai.response.id"
	attrFinishReasons = "gen_ai.response.finish_reasons"
	attrFirstChunk    = "gen_ai.response.time_to_first_chunk"
	attrConversation  = "gen_ai.conversation.id"
	attrServerAddress = "server.address"
	attrHTTPStatus    = "http.response.status_code"
	attrErrorType     = "error.type"

	attrInputTokens     = "gen_ai.usage.input_tokens"
	attrOutputTokens    = "gen_ai.usage.output_tokens"
	attrCacheRead       = "gen_ai.usage.cache_read.input_tokens"
	attrCacheWrite      = "gen_ai.usage.cache_write.input_tokens"
	attrReasoningTokens = "gen_ai.usage.reasoning.output_tokens"
	attrAudioInput      = "gen_ai.usage.audio.input_tokens"
	attrAudioOutput     = "gen_ai.usage.audio.output_tokens"

	// attrRelayHost is relay's own attribute, not a convention: the catalog host the call was resolved to.
	attrRelayHost = "wyolet.relay.host"
)

// eventInferenceDetails is the conventions' event for a model call; it carries the content.
const eventInferenceDetails = "gen_ai.client.inference.operation.details"

// callAttributes returns the attributes the span and the content event of a call share.
func callAttributes(c *Call) []keyValue {
	op := c.Operation
	if op == "" {
		op = OperationChat
	}
	attrs := make([]keyValue, 0, 24)
	attrs = append(attrs, stringAttr(attrOperation, op))
	add := func(k, v string) {
		if v != "" {
			attrs = append(attrs, stringAttr(k, v))
		}
	}
	add(attrProvider, c.Provider)
	add(attrRequestModel, c.RequestModel)
	attrs = append(attrs, boolAttr(attrStream, c.Stream))
	add(attrResponseModel, c.ResponseModel)
	add(attrResponseID, c.ResponseID)
	if len(c.FinishReasons) > 0 {
		attrs = append(attrs, stringsAttr(attrFinishReasons, append([]string(nil), c.FinishReasons...)))
	}
	if c.Stream && c.TimeToFirstChunk > 0 {
		attrs = append(attrs, doubleAttr(attrFirstChunk, c.TimeToFirstChunk.Seconds()))
	}
	add(attrConversation, c.ConversationID)
	add(attrServerAddress, c.ServerAddress)
	if c.StatusCode != 0 {
		attrs = append(attrs, intAttr(attrHTTPStatus, int64(c.StatusCode)))
	}
	add(attrErrorType, c.ErrorType)
	attrs = appendUsage(attrs, c.Usage)
	add(attrRelayHost, c.Host)
	return attrs
}

// appendUsage maps canonical token counts to the conventions' usage attributes. Canonical input excludes cached tokens while the conventions' input_tokens includes them, so it is the sum of the three; output already includes reasoning and audio in both.
//
// canonical: accepted_prediction, rejected_prediction, cache_creation_1h, server_tool_use_input, server_tool_use_output dropped — the conventions have no attribute for them; the first two stay counted inside output, cache_creation_1h inside cache_creation.
func appendUsage(attrs []keyValue, t usage.Tokens) []keyValue {
	input, hasInput := int64(0), false
	for _, k := range [...]string{"input", "cache_read", "cache_creation"} {
		if n, ok := t[k]; ok {
			input += n
			hasInput = true
		}
	}
	if hasInput {
		attrs = append(attrs, intAttr(attrInputTokens, input))
	}
	for _, m := range [...]struct{ key, attr string }{
		{"output", attrOutputTokens},
		{"cache_read", attrCacheRead},
		{"cache_creation", attrCacheWrite},
		{"reasoning", attrReasoningTokens},
		{"audio_input", attrAudioInput},
		{"audio_output", attrAudioOutput},
	} {
		if n, ok := t[m.key]; ok {
			attrs = append(attrs, intAttr(m.attr, n))
		}
	}
	return attrs
}

// record is a call ready for export: everything taken from the caller's values is copied or encoded.
type record struct {
	traceID, spanID, parentID string
	name                      string
	start, end                time.Time
	attrs                     []keyValue
	failed                    bool
	content                   *Content
}

func newRecord(c *Call) *record {
	r := &record{
		name:    spanName(c),
		start:   c.Start,
		end:     c.End,
		attrs:   callAttributes(c),
		failed:  c.ErrorType != "",
		content: c.Content,
	}
	if r.start.IsZero() {
		r.start = time.Now()
	}
	if r.end.Before(r.start) {
		r.end = r.start
	}
	r.traceID, r.spanID, r.parentID = spanIDs(c.Parent)
	return r
}

func spanName(c *Call) string {
	op := c.Operation
	if op == "" {
		op = OperationChat
	}
	if c.RequestModel == "" {
		return op
	}
	return op + " " + c.RequestModel
}

func (r *record) span() span {
	s := span{
		TraceID:           r.traceID,
		SpanID:            r.spanID,
		ParentSpanID:      r.parentID,
		Name:              r.name,
		Kind:              spanKindClient,
		StartTimeUnixNano: unixNano(r.start.UnixNano()),
		EndTimeUnixNano:   unixNano(r.end.UnixNano()),
		Attributes:        r.attrs,
	}
	if r.failed {
		s.Status = &status{Code: statusError}
	}
	return s
}

// event is the call's inference details event: the span's attributes plus the content, so the call is complete whichever of the two a receiver sees first.
func (r *record) event() logRecord {
	attrs := make([]keyValue, 0, len(r.attrs)+len(r.content.attrs))
	attrs = append(append(attrs, r.attrs...), r.content.attrs...)
	ts := unixNano(r.end.UnixNano())
	return logRecord{
		TimeUnixNano:         unixNano(r.start.UnixNano()),
		ObservedTimeUnixNano: ts,
		SeverityNumber:       severityDebug,
		EventName:            eventInferenceDetails,
		TraceID:              r.traceID,
		SpanID:               r.spanID,
		Attributes:           attrs,
	}
}
