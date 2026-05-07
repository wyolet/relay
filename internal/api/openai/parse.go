package openai

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/wyolet/relay/internal/usage"
)

var richParsing = true // default on; set via SetRichParsing at boot

// SetRichParsing controls whether Parse extracts metadata and messages.
// Must be called before any request is served (boot-time only).
func SetRichParsing(on bool) { richParsing = on }

// RichParsing reports the current toggle state.
func RichParsing() bool { return richParsing }

// parseError carries a ready-to-write HTTP status code and JSON body.
type parseError struct {
	status int
	body   []byte
}

func (e *parseError) Error() string { return string(e.body) }

func newParseError(status int, errType, code, msg string) *parseError {
	b, _ := json.Marshal(errEnvelope{Error: errBody{Message: msg, Type: errType, Code: code}})
	return &parseError{status: status, body: b}
}

// ParseError returns the HTTP status and JSON envelope body if err is a *parseError,
// otherwise reports ok=false.
func ParseError(err error) (status int, body []byte, ok bool) {
	var pe *parseError
	if e, is := err.(*parseError); is {
		pe = e
	}
	if pe == nil {
		return 0, nil, false
	}
	return pe.status, pe.body, true
}

// wireBody is the minimal shape we unmarshal into for both rich and minimal modes.
type wireBody struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	User     string            `json:"user"`
	Metadata map[string]string `json:"metadata"`
	Messages []json.RawMessage `json:"messages"`
}

// Parse decodes body into a ChatRequest.
//
// Always extracted: Model (required), Stream, User, Raw.
// Rich mode only: Metadata (validated, dropped on caps violation), Messages.
// Minimal mode: Metadata and Messages left nil.
//
// On error returns a *parseError (use ParseError to unpack status + body).
func Parse(_ context.Context, body []byte, _ http.Header) (*ChatRequest, error) {
	var w wireBody
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, newParseError(http.StatusBadRequest, "invalid_request_error", "invalid_body", "request body is not valid JSON")
	}
	if w.Model == "" {
		return nil, newParseError(http.StatusBadRequest, "invalid_request_error", "missing_model", "model is required")
	}

	cr := &ChatRequest{
		Model:  w.Model,
		Stream: w.Stream,
		User:   w.User,
		Raw:    json.RawMessage(body),
	}

	if richParsing {
		if len(w.Metadata) > 0 {
			cr.Metadata = validateBodyMetadata(w.Metadata)
		}
		cr.Messages = w.Messages
	}

	return cr, nil
}

// validateBodyMetadata applies the same caps as ParseMetadataHeader.
// Returns nil on any violation (drop-on-violation policy).
func validateBodyMetadata(m map[string]string) map[string]string {
	if len(m) > 16 {
		incBodyRejected(usage.ReasonOversize)
		return nil
	}
	for k, v := range m {
		if len(k) > 64 || len(v) > 256 {
			incBodyRejected(usage.ReasonOversize)
			return nil
		}
		if !validMetaKey(k) {
			incBodyRejected(usage.ReasonBadCharset)
			return nil
		}
		if !validMetaValue(v) {
			incBodyRejected(usage.ReasonBadCharset)
			return nil
		}
	}
	return m
}

// incBodyRejected increments the body-metadata rejection counter.
// We reuse the usage package's reason constants but track body rejections
// separately to avoid conflating header vs body drops in metrics.
// For now we just log via the package-level atomic in usage — this is a
// thin wrapper kept for future per-source metrics separation.
func incBodyRejected(_ string) {
	// deliberate no-op for now; callers will hook Prometheus in a future ticket
}

func validMetaKey(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func validMetaValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7E || c == ',' || c == '=' {
			return false
		}
	}
	return true
}
