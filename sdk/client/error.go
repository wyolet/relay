package client

import (
	"encoding/json"
	"fmt"
)

// APIError is a non-2xx response from the target. Host names the upstream
// the call actually hit (a relay deployment or a vendor host dialed
// directly), so the error self-identifies its origin.
type APIError struct {
	StatusCode int
	Host       string
	Code       string
	Message    string
	Raw        []byte
}

func (e *APIError) Error() string {
	host := e.Host
	if host == "" {
		host = "upstream"
	}
	if e.Code != "" {
		return fmt.Sprintf("%s: %d %s: %s", host, e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %d: %s", host, e.StatusCode, string(e.Raw))
}

// parseAPIError best-effort-extracts a code/message from the common
// {"error":{...}} envelope (relay, OpenAI, Anthropic all use it).
func parseAPIError(host string, status int, body []byte) *APIError {
	e := &APIError{StatusCode: status, Host: host, Raw: body}
	var wire struct {
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &wire) == nil {
		e.Code = wire.Error.Code
		if e.Code == "" {
			e.Code = wire.Error.Type // Anthropic uses error.type
		}
		e.Message = wire.Error.Message
	}
	return e
}
