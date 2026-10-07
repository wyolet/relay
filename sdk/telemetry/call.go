package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/wyolet/relay/sdk/usage"
)

// Operation names a model call kind in the conventions' gen_ai.operation.name.
const (
	OperationChat       = "chat"
	OperationEmbeddings = "embeddings"
)

// Error types for a call that failed without an HTTP error status. They are the words relay's receiver classifies, and stable across SDK releases.
const (
	ErrorTimeout     = "timeout"
	ErrorCanceled    = "client_canceled"
	ErrorUnreachable = "upstream_unreachable"
	// ErrorOther is the conventions' value for an error with no more specific type.
	ErrorOther = "_OTHER"
)

// FinishError is the finish reason the conventions ask for when a response ended before its final event.
const FinishError = "error"

// Call is one model call as it is reported. Fields left zero are not reported.
type Call struct {
	// Operation is OperationChat or OperationEmbeddings; empty means chat.
	Operation string
	// Provider is the provider name, a catalog provider slug where one is known.
	Provider string
	// Host is the catalog host slug the call was resolved to, so relay prices it at that host.
	Host string
	// ServerAddress is the host name the call was sent to.
	ServerAddress string

	RequestModel  string
	ResponseModel string
	ResponseID    string
	FinishReasons []string
	Stream        bool
	// TimeToFirstChunk is measured from Start; streams only.
	TimeToFirstChunk time.Duration

	Start time.Time
	End   time.Time

	// StatusCode is the provider's HTTP status, 0 when no response arrived.
	StatusCode int
	// ErrorType is set when the call failed: the HTTP error status as text, or one of the Error constants.
	ErrorType string

	// Usage is the canonical token map the SDK's translators produce.
	Usage usage.Tokens

	// Parent is a W3C traceparent the call's span joins; see ContextWithTraceparent.
	Parent         string
	ConversationID string

	// Content is the call's message content, nil when none is sent.
	Content *Content
}

type contextKey int

const (
	traceparentKey contextKey = iota
	conversationKey
)

// ContextWithTraceparent returns ctx carrying a W3C traceparent header value. A call made with it joins that trace, its span a child of the traceparent's span. A malformed value is ignored when the call is recorded.
func ContextWithTraceparent(ctx context.Context, traceparent string) context.Context {
	return context.WithValue(ctx, traceparentKey, traceparent)
}

// TraceparentFromContext returns the traceparent ContextWithTraceparent stored, or "".
func TraceparentFromContext(ctx context.Context) string {
	v, _ := ctx.Value(traceparentKey).(string)
	return v
}

// ContextWithConversation returns ctx carrying a conversation (session, thread) id, reported as gen_ai.conversation.id.
func ContextWithConversation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, conversationKey, id)
}

// ConversationFromContext returns the id ContextWithConversation stored, or "".
func ConversationFromContext(ctx context.Context) string {
	v, _ := ctx.Value(conversationKey).(string)
	return v
}

// spanIDs returns the trace id, span id and parent span id for a call: the traceparent's trace and span when it is valid, a new trace otherwise.
func spanIDs(traceparent string) (traceID, spanID, parentID string) {
	spanID = randomHex(8)
	if t, p, ok := parseTraceparent(traceparent); ok {
		return t, spanID, p
	}
	return randomHex(16), spanID, ""
}

// parseTraceparent reads version 00 of the W3C header ("00-<32 hex>-<16 hex>-<2 hex>"), rejecting the all-zero ids the format declares invalid.
func parseTraceparent(v string) (traceID, spanID string, ok bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) != 4 || parts[0] != "00" || !isHexID(parts[1], 32) || !isHexID(parts[2], 16) || len(parts[3]) != 2 {
		return "", "", false
	}
	return strings.ToLower(parts[1]), strings.ToLower(parts[2]), true
}

func isHexID(s string, length int) bool {
	if len(s) != length || strings.Trim(s, "0") == "" {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
