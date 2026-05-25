package openai

import (
	"encoding/json"
	"fmt"
)

// ResponsesMessage is an input or output message item (role + content).
type ResponsesMessage struct {
	ID      string          `json:"id,omitempty"`
	Status  ResponsesStatus `json:"status,omitempty"`
	Role    ResponsesRole   `json:"role"`
	Content []ResponsesPart `json:"-"` // normalized; use MarshalJSON/UnmarshalJSON
}

func (*ResponsesMessage) isResponsesItem()                     {}
func (*ResponsesMessage) ResponsesItemType() ResponsesItemType { return ResponsesItemTypeMessage }

// MarshalJSON emits the wire shape with content as a typed array.
func (m *ResponsesMessage) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type    ResponsesItemType `json:"type"`
		ID      string            `json:"id,omitempty"`
		Status  ResponsesStatus   `json:"status,omitempty"`
		Role    ResponsesRole     `json:"role"`
		Content []ResponsesPart   `json:"content"`
	}
	return json.Marshal(wire{
		Type:    ResponsesItemTypeMessage,
		ID:      m.ID,
		Status:  m.Status,
		Role:    m.Role,
		Content: m.Content,
	})
}

// UnmarshalJSON is handled by responsesUnmarshalItem; this exists for direct decode use.
func (m *ResponsesMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID      string          `json:"id"`
		Status  ResponsesStatus `json:"status"`
		Role    ResponsesRole   `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.ID = raw.ID
	m.Status = raw.Status
	m.Role = raw.Role
	parts, err := responsesUnmarshalContent(raw.Content)
	if err != nil {
		return err
	}
	m.Content = parts
	return nil
}

// ResponsesFunctionCall is an output item representing a tool invocation by the model.
type ResponsesFunctionCall struct {
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"` // JSON string
	Status    ResponsesStatus `json:"status,omitempty"`
}

func (*ResponsesFunctionCall) isResponsesItem() {}
func (*ResponsesFunctionCall) ResponsesItemType() ResponsesItemType {
	return ResponsesItemTypeFunctionCall
}

func (f *ResponsesFunctionCall) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type      ResponsesItemType `json:"type"`
		ID        string            `json:"id,omitempty"`
		CallID    string            `json:"call_id"`
		Name      string            `json:"name"`
		Arguments string            `json:"arguments"`
		Status    ResponsesStatus   `json:"status,omitempty"`
	}
	return json.Marshal(wire{
		Type:      ResponsesItemTypeFunctionCall,
		ID:        f.ID,
		CallID:    f.CallID,
		Name:      f.Name,
		Arguments: f.Arguments,
		Status:    f.Status,
	})
}

// ResponsesFunctionCallOutput is an input item that delivers the result of a tool call.
// Exactly one of Output (string) or Content ([]ResponsesPart) is set on a valid item.
type ResponsesFunctionCallOutput struct {
	CallID  string          `json:"call_id"`
	Output  string          `json:"-"` // string form
	Content []ResponsesPart `json:"-"` // array form; mutually exclusive with Output
}

func (*ResponsesFunctionCallOutput) isResponsesItem() {}
func (*ResponsesFunctionCallOutput) ResponsesItemType() ResponsesItemType {
	return ResponsesItemTypeFunctionCallOutput
}

func (f *ResponsesFunctionCallOutput) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type    ResponsesItemType `json:"type"`
		CallID  string            `json:"call_id"`
		Output  string            `json:"output,omitempty"`
		Content []ResponsesPart   `json:"content,omitempty"`
	}
	return json.Marshal(wire{
		Type:    ResponsesItemTypeFunctionCallOutput,
		CallID:  f.CallID,
		Output:  f.Output,
		Content: f.Content,
	})
}

func (f *ResponsesFunctionCallOutput) UnmarshalJSON(data []byte) error {
	var raw struct {
		CallID  string          `json:"call_id"`
		Output  json.RawMessage `json:"output"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.CallID = raw.CallID
	if len(raw.Output) > 0 {
		if err := json.Unmarshal(raw.Output, &f.Output); err != nil {
			return fmt.Errorf("function_call_output.output: %w", err)
		}
	}
	if len(raw.Content) > 0 && string(raw.Content) != "null" {
		parts, err := responsesUnmarshalContent(raw.Content)
		if err != nil {
			return err
		}
		f.Content = parts
	}
	return nil
}

// ResponsesSummaryText is one element of a ResponsesReasoning item's summary array.
type ResponsesSummaryText struct {
	Text string `json:"text"`
}

// ResponsesReasoning is an output item representing the model's reasoning steps.
type ResponsesReasoning struct {
	ID               string                 `json:"id,omitempty"`
	Summary          []ResponsesSummaryText `json:"summary,omitempty"`
	EncryptedContent string                 `json:"encrypted_content,omitempty"`
	Status           ResponsesStatus        `json:"status,omitempty"`
}

func (*ResponsesReasoning) isResponsesItem()                     {}
func (*ResponsesReasoning) ResponsesItemType() ResponsesItemType { return ResponsesItemTypeReasoning }

func (r *ResponsesReasoning) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type             ResponsesItemType      `json:"type"`
		ID               string                 `json:"id,omitempty"`
		Summary          []ResponsesSummaryText `json:"summary,omitempty"`
		EncryptedContent string                 `json:"encrypted_content,omitempty"`
		Status           ResponsesStatus        `json:"status,omitempty"`
	}
	return json.Marshal(wire{
		Type:             ResponsesItemTypeReasoning,
		ID:               r.ID,
		Summary:          r.Summary,
		EncryptedContent: r.EncryptedContent,
		Status:           r.Status,
	})
}

// responsesUnmarshalItem decodes a single item from the wire format.
func responsesUnmarshalItem(data []byte) (ResponsesItem, error) {
	var probe struct {
		Type ResponsesItemType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}
	switch probe.Type {
	case ResponsesItemTypeMessage:
		var v ResponsesMessage
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("message item: %w", err)
		}
		return &v, nil
	case ResponsesItemTypeFunctionCall:
		var v ResponsesFunctionCall
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("function_call item: %w", err)
		}
		return &v, nil
	case ResponsesItemTypeFunctionCallOutput:
		var v ResponsesFunctionCallOutput
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("function_call_output item: %w", err)
		}
		return &v, nil
	case ResponsesItemTypeReasoning:
		var v ResponsesReasoning
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("reasoning item: %w", err)
		}
		return &v, nil
	default:
		return nil, fmt.Errorf("unsupported item type %q", probe.Type)
	}
}

// responsesUnmarshalItems decodes an array of items from wire JSON.
func responsesUnmarshalItems(data []byte) ([]ResponsesItem, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("input array: %w", err)
	}
	items := make([]ResponsesItem, 0, len(raws))
	for _, raw := range raws {
		it, err := responsesUnmarshalItem(raw)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}
