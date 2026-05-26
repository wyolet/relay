package v1

import (
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/pkg/usage"
)

// Marshal encodes a Response to wire JSON.
// Output items are marshaled via their individual MarshalJSON implementations.
func Marshal(resp *Response) ([]byte, error) {
	return json.Marshal(resp)
}

// UnmarshalResponse decodes a wire JSON response into a *Response.
// Output items are dispatched via unmarshalItem.
func UnmarshalResponse(data []byte) (*Response, error) {
	var wire struct {
		ID                string                     `json:"id"`
		Object            string                     `json:"object"`
		CreatedAt         int64                      `json:"created_at"`
		Model             string                     `json:"model"`
		Status            Status                     `json:"status"`
		FinishReason      FinishReason               `json:"finish_reason"`
		Output            []json.RawMessage          `json:"output"`
		Usage             usage.Tokens               `json:"usage"`
		Error             *Error                     `json:"error"`
		IncompleteDetails *IncompleteDetails         `json:"incomplete_details"`
		Extensions        map[string]json.RawMessage `json:"extensions"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, fmt.Errorf("response: %w", err)
	}

	output := make([]Item, 0, len(wire.Output))
	for i, raw := range wire.Output {
		item, err := unmarshalItem(raw)
		if err != nil {
			return nil, fmt.Errorf("output[%d]: %w", i, err)
		}
		output = append(output, item)
	}

	return &Response{
		ID:                wire.ID,
		Object:            wire.Object,
		CreatedAt:         wire.CreatedAt,
		Model:             wire.Model,
		Status:            wire.Status,
		FinishReason:      wire.FinishReason,
		Output:            output,
		Usage:             wire.Usage,
		Error:             wire.Error,
		IncompleteDetails: wire.IncompleteDetails,
		Extensions:        wire.Extensions,
	}, nil
}
