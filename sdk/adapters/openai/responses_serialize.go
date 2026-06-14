package openai

import (
	"encoding/json"
	"fmt"
)

// MarshalResponsesResponse encodes a ResponsesResponse to wire JSON via the struct's MarshalJSON.
func MarshalResponsesResponse(resp *ResponsesResponse) ([]byte, error) {
	return json.Marshal(resp)
}

// UnmarshalResponsesResponse decodes a wire JSON response into a *ResponsesResponse.
// Output items are dispatched via responsesUnmarshalItem.
func UnmarshalResponsesResponse(data []byte) (*ResponsesResponse, error) {
	var wire struct {
		ID                string                      `json:"id"`
		Object            string                      `json:"object"`
		CreatedAt         int64                       `json:"created_at"`
		Model             string                      `json:"model"`
		Status            ResponsesStatus             `json:"status"`
		Output            []json.RawMessage           `json:"output"`
		Usage             *ResponsesUsage             `json:"usage"`
		Error             *ResponsesError             `json:"error"`
		IncompleteDetails *ResponsesIncompleteDetails `json:"incomplete_details"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("response: %w", err)
	}

	output := make([]ResponsesItem, 0, len(wire.Output))
	for i, raw := range wire.Output {
		item, err := responsesUnmarshalItem(raw)
		if err != nil {
			return nil, fmt.Errorf("output[%d]: %w", i, err)
		}
		output = append(output, item)
	}

	return &ResponsesResponse{
		ID:                wire.ID,
		Object:            wire.Object,
		CreatedAt:         wire.CreatedAt,
		Model:             wire.Model,
		Status:            wire.Status,
		Output:            output,
		Usage:             wire.Usage,
		Error:             wire.Error,
		IncompleteDetails: wire.IncompleteDetails,
	}, nil
}
