package anthropic

import (
	"encoding/json"
	"net/http"
)

// parseError carries a ready-to-write HTTP status code and JSON body.
type parseError struct {
	status int
	body   []byte
}

func (e *parseError) Error() string { return string(e.body) }

func newParseError(status int, errType, msg string) *parseError {
	b, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
	return &parseError{status: status, body: b}
}

// ParseError returns the HTTP status and JSON envelope body if err is a *parseError,
// otherwise reports ok=false.
func ParseError(err error) (status int, body []byte, ok bool) {
	if pe, is := err.(*parseError); is {
		return pe.status, pe.body, true
	}
	return 0, nil, false
}

// wireBody is the minimal shape we unmarshal into for parsing.
type wireBody struct {
	Model     string            `json:"model"`
	Stream    bool              `json:"stream"`
	MaxTokens int               `json:"max_tokens"`
	Metadata  map[string]any    `json:"metadata"`
	Messages  []json.RawMessage `json:"messages"`
}

// Parse decodes body into a MessagesRequest.
// Always extracted: Model (required), Stream, MaxTokens, Raw.
// Messages are extracted as raw JSON (not deep-parsed).
func Parse(body []byte) (*MessagesRequest, error) {
	var w wireBody
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, newParseError(http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON")
	}
	if w.Model == "" {
		return nil, newParseError(http.StatusBadRequest, "invalid_request_error", "model is required")
	}
	if w.MaxTokens == 0 {
		return nil, newParseError(http.StatusBadRequest, "invalid_request_error", "max_tokens is required")
	}

	mr := &MessagesRequest{
		Model:     w.Model,
		Stream:    w.Stream,
		MaxTokens: w.MaxTokens,
		Messages:  w.Messages,
		Raw:       json.RawMessage(body),
	}

	// Extract user_id from metadata if present (Anthropic-specific attribution field).
	if w.Metadata != nil {
		if uid, ok := w.Metadata["user_id"]; ok {
			if s, ok := uid.(string); ok && s != "" {
				mr.Metadata = map[string]string{"user_id": s}
			}
		}
	}

	return mr, nil
}
