package responses

import (
	"encoding/json"
	"fmt"
)

// Message is an input or output message item (role + content).
type Message struct {
	ID      string  `json:"id,omitempty"`
	Status  Status  `json:"status,omitempty"`
	Role    Role    `json:"role"`
	Content []Part  `json:"-"` // normalized; use MarshalJSON/UnmarshalJSON
}

func (*Message) isItem()             {}
func (*Message) ItemType() ItemType  { return ItemTypeMessage }

// MarshalJSON emits the wire shape with content as a typed array.
func (m *Message) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type    ItemType        `json:"type"`
		ID      string          `json:"id,omitempty"`
		Status  Status          `json:"status,omitempty"`
		Role    Role            `json:"role"`
		Content []Part          `json:"content"`
	}
	return json.Marshal(wire{
		Type:    ItemTypeMessage,
		ID:      m.ID,
		Status:  m.Status,
		Role:    m.Role,
		Content: m.Content,
	})
}

// UnmarshalJSON is handled by unmarshalItem; this exists for direct decode use.
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID      string          `json:"id"`
		Status  Status          `json:"status"`
		Role    Role            `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.ID = raw.ID
	m.Status = raw.Status
	m.Role = raw.Role
	parts, err := unmarshalContent(raw.Content)
	if err != nil {
		return err
	}
	m.Content = parts
	return nil
}

// FunctionCall is an output item representing a tool invocation by the model.
type FunctionCall struct {
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
	Status    Status `json:"status,omitempty"`
}

func (*FunctionCall) isItem()            {}
func (*FunctionCall) ItemType() ItemType { return ItemTypeFunctionCall }

func (f *FunctionCall) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type      ItemType `json:"type"`
		ID        string   `json:"id,omitempty"`
		CallID    string   `json:"call_id"`
		Name      string   `json:"name"`
		Arguments string   `json:"arguments"`
		Status    Status   `json:"status,omitempty"`
	}
	return json.Marshal(wire{
		Type:      ItemTypeFunctionCall,
		ID:        f.ID,
		CallID:    f.CallID,
		Name:      f.Name,
		Arguments: f.Arguments,
		Status:    f.Status,
	})
}

// FunctionCallOutput is an input item that delivers the result of a tool call.
// Exactly one of Output (string) or Content ([]Part) is set on a valid item.
type FunctionCallOutput struct {
	CallID  string `json:"call_id"`
	Output  string `json:"-"` // string form
	Content []Part `json:"-"` // array form; mutually exclusive with Output
}

func (*FunctionCallOutput) isItem()            {}
func (*FunctionCallOutput) ItemType() ItemType { return ItemTypeFunctionCallOutput }

func (f *FunctionCallOutput) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type    ItemType        `json:"type"`
		CallID  string          `json:"call_id"`
		Output  string          `json:"output,omitempty"`
		Content []Part          `json:"content,omitempty"`
	}
	return json.Marshal(wire{
		Type:    ItemTypeFunctionCallOutput,
		CallID:  f.CallID,
		Output:  f.Output,
		Content: f.Content,
	})
}

func (f *FunctionCallOutput) UnmarshalJSON(data []byte) error {
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
		parts, err := unmarshalContent(raw.Content)
		if err != nil {
			return err
		}
		f.Content = parts
	}
	return nil
}

// SummaryText is one element of a Reasoning item's summary array.
type SummaryText struct {
	Text string `json:"text"`
}

// Reasoning is an output item representing the model's reasoning steps.
type Reasoning struct {
	ID               string        `json:"id,omitempty"`
	Summary          []SummaryText `json:"summary,omitempty"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
	Status           Status        `json:"status,omitempty"`
}

func (*Reasoning) isItem()            {}
func (*Reasoning) ItemType() ItemType { return ItemTypeReasoning }

func (r *Reasoning) MarshalJSON() ([]byte, error) {
	type wire struct {
		Type             ItemType      `json:"type"`
		ID               string        `json:"id,omitempty"`
		Summary          []SummaryText `json:"summary,omitempty"`
		EncryptedContent string        `json:"encrypted_content,omitempty"`
		Status           Status        `json:"status,omitempty"`
	}
	return json.Marshal(wire{
		Type:             ItemTypeReasoning,
		ID:               r.ID,
		Summary:          r.Summary,
		EncryptedContent: r.EncryptedContent,
		Status:           r.Status,
	})
}

// unmarshalItem decodes a single item from the wire format.
// Returns an explicit error for unsupported item types.
func unmarshalItem(data []byte) (Item, error) {
	var probe struct {
		Type ItemType `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("item: %w", err)
	}
	switch probe.Type {
	case ItemTypeMessage:
		var v Message
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("message item: %w", err)
		}
		return &v, nil
	case ItemTypeFunctionCall:
		var v FunctionCall
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("function_call item: %w", err)
		}
		return &v, nil
	case ItemTypeFunctionCallOutput:
		var v FunctionCallOutput
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("function_call_output item: %w", err)
		}
		return &v, nil
	case ItemTypeReasoning:
		var v Reasoning
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, fmt.Errorf("reasoning item: %w", err)
		}
		return &v, nil
	default:
		return nil, fmt.Errorf("unsupported item type %q", probe.Type)
	}
}

// unmarshalItems decodes an array of items from wire JSON.
func unmarshalItems(data []byte) ([]Item, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("input array: %w", err)
	}
	items := make([]Item, 0, len(raws))
	for _, raw := range raws {
		it, err := unmarshalItem(raw)
		if err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, nil
}
