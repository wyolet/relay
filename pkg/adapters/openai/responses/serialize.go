package responses

import (
	"encoding/json"
	"fmt"
)

// Marshal encodes a Response to wire JSON.
// Output items are marshaled via their individual MarshalJSON implementations.
func Marshal(resp *Response) ([]byte, error) {
	// wireResponse is the flat wire shape. Output is encoded as a raw array
	// to delegate to each Item's MarshalJSON.
	type wireResponse struct {
		ID                string             `json:"id"`
		Object            string             `json:"object"`
		CreatedAt         int64              `json:"created_at"`
		Model             string             `json:"model"`
		Status            Status             `json:"status"`
		FinishReason      FinishReason       `json:"finish_reason,omitempty"`
		Output            []json.RawMessage  `json:"output"`
		Usage             *Usage             `json:"usage,omitempty"`
		Error             *Error             `json:"error,omitempty"`
		IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
	}

	outputRaws := make([]json.RawMessage, len(resp.Output))
	for i, item := range resp.Output {
		b, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("output[%d]: %w", i, err)
		}
		outputRaws[i] = b
	}

	return json.Marshal(wireResponse{
		ID:                resp.ID,
		Object:            resp.Object,
		CreatedAt:         resp.CreatedAt,
		Model:             resp.Model,
		Status:            resp.Status,
		FinishReason:      resp.FinishReason,
		Output:            outputRaws,
		Usage:             resp.Usage,
		Error:             resp.Error,
		IncompleteDetails: resp.IncompleteDetails,
	})
}

// UnmarshalResponse decodes a wire JSON response into a *Response.
// Output items are dispatched via unmarshalItem.
func UnmarshalResponse(data []byte) (*Response, error) {
	var wire struct {
		ID                string             `json:"id"`
		Object            string             `json:"object"`
		CreatedAt         int64              `json:"created_at"`
		Model             string             `json:"model"`
		Status            Status             `json:"status"`
		FinishReason      FinishReason       `json:"finish_reason"`
		Output            []json.RawMessage  `json:"output"`
		Usage             *Usage             `json:"usage"`
		Error             *Error             `json:"error"`
		IncompleteDetails *IncompleteDetails `json:"incomplete_details"`
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
	}, nil
}
