package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

type anthropicStreamBlock struct {
	blockType   string // "text", "tool_use", "thinking"
	outputIndex int
	itemID      string
	textBuf     strings.Builder
	argsBuf     strings.Builder
	thinkBuf    strings.Builder
	// sigBuf accumulates signature_delta chunks for thinking blocks. Anthropic
	// requires the signature to round-trip in multi-turn extended thinking.
	sigBuf   strings.Builder
	callID   string
	toolName string
	// redacted is the whole item for a redacted_thinking block: its data arrives on the start frame, never in deltas.
	redacted *v1.Reasoning
}

func (s *anthropicToCanonicalStream) handleContentBlockStart(data []byte) ([]byte, error) {
	var cbs struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id,omitempty"`
			Name string `json:"name,omitempty"`
			Data string `json:"data,omitempty"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal(data, &cbs); err != nil {
		return nil, fmt.Errorf("anthropic stream: content_block_start: %w", err)
	}

	blockType := cbs.ContentBlock.Type
	if blockType == "" {
		return nil, nil
	}

	// canonical: server_tool_use blocks dropped on the stream — no canonical
	// item for a hosted tool call; the buffered path drops them as well.
	if blockType == "server_tool_use" {
		return nil, nil
	}

	outputIndex := s.nextIndex
	s.nextIndex++

	b := &anthropicStreamBlock{
		blockType:   blockType,
		outputIndex: outputIndex,
		itemID:      fmt.Sprintf("%s_%d", string([]rune(blockType)[:1]), outputIndex),
	}

	switch blockType {
	case "tool_use":
		b.callID = cbs.ContentBlock.ID
		b.toolName = cbs.ContentBlock.Name
	case "redacted_thinking":
		b.redacted = redactedThinkingToReasoning(cbs.ContentBlock.Data)
	}

	s.currentBlock = b

	var itemType v1.ItemType
	switch blockType {
	case "text":
		itemType = v1.ItemTypeMessage
	case "tool_use":
		itemType = v1.ItemTypeFunctionCall
	case "thinking", "redacted_thinking":
		itemType = v1.ItemTypeReasoning
	default:
		// canonical: unmodeled content blocks (tool results of hosted tools,
		// future types) dropped — no canonical item type to start.
		s.currentBlock = nil
		return nil, nil
	}

	started := v1.ItemStartedEvent{
		ItemID:   b.itemID,
		ItemType: itemType,
		Name:     b.toolName,
		CallID:   b.callID,
		Index:    outputIndex,
	}
	if b.redacted != nil {
		started.ProviderData = b.redacted.ProviderData
	}
	startData, _ := json.Marshal(started)
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemStarted, Data: startData}}), nil
}

func (s *anthropicToCanonicalStream) handleContentBlockDelta(data []byte) ([]byte, error) {
	if s.currentBlock == nil {
		return nil, nil
	}

	var cbd struct {
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text,omitempty"`
			PartialJSON string `json:"partial_json,omitempty"`
			Thinking    string `json:"thinking,omitempty"`
			Signature   string `json:"signature,omitempty"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &cbd); err != nil {
		return nil, fmt.Errorf("anthropic stream: content_block_delta: %w", err)
	}

	b := s.currentBlock
	var kind v1.DeltaKind
	var deltaText string

	switch cbd.Delta.Type {
	case "text_delta":
		b.textBuf.WriteString(cbd.Delta.Text)
		kind = v1.DeltaKindText
		deltaText = cbd.Delta.Text
	case "input_json_delta":
		b.argsBuf.WriteString(cbd.Delta.PartialJSON)
		kind = v1.DeltaKindArguments
		deltaText = cbd.Delta.PartialJSON
	case "thinking_delta":
		b.thinkBuf.WriteString(cbd.Delta.Thinking)
		kind = v1.DeltaKindReasoning
		deltaText = cbd.Delta.Thinking
	case "signature_delta":
		// Accumulate the thinking signature; Anthropic requires it to round-trip
		// for multi-turn extended thinking. No canonical delta is emitted — the
		// signature surfaces only in the completed Reasoning.ProviderData.
		b.sigBuf.WriteString(cbd.Delta.Signature)
		return nil, nil
	default:
		// canonical: unmodeled delta types (citations_delta, future types)
		// dropped — canonical deltas are text, arguments, or reasoning only.
		return nil, nil
	}

	deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
		ItemID: b.itemID,
		Index:  b.outputIndex,
		Kind:   kind,
		Delta:  deltaText,
	})
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemDelta, Data: deltaData}}), nil
}

func (s *anthropicToCanonicalStream) handleContentBlockStop(_ []byte) ([]byte, error) {
	if s.currentBlock == nil {
		return nil, nil
	}
	b := s.currentBlock
	s.currentBlock = nil

	var completedItem v1.Item
	switch b.blockType {
	case "text":
		completedItem = &v1.Message{
			ID:      b.itemID,
			Role:    v1.RoleAssistant,
			Status:  v1.StatusCompleted,
			Content: []v1.Part{&v1.OutputTextPart{Text: b.textBuf.String()}},
		}
	case "tool_use":
		completedItem = &v1.FunctionCall{
			ID:        b.itemID,
			CallID:    b.callID,
			Name:      b.toolName,
			Arguments: b.argsBuf.String(),
			Status:    functionCallStatus(b.argsBuf.String()),
		}
	case "thinking":
		thinkText := b.thinkBuf.String()
		// Build ProviderData in the same shape as ParseResponse so that
		// multi-turn round-trips are consistent regardless of sync vs stream path.
		var providerData json.RawMessage
		if sig := b.sigBuf.String(); sig != "" {
			pd := map[string]string{
				"type":      "thinking",
				"thinking":  thinkText,
				"signature": sig,
			}
			providerData, _ = json.Marshal(pd)
		}
		completedItem = &v1.Reasoning{
			ID:           b.itemID,
			Content:      thinkText,
			Summary:      []v1.SummaryText{{Text: thinkText}},
			Status:       v1.StatusCompleted,
			ProviderData: providerData,
		}
	case "redacted_thinking":
		b.redacted.ID, b.redacted.Status = b.itemID, v1.StatusCompleted
		completedItem = b.redacted
	default:
		return nil, nil
	}

	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: b.itemID,
		Index:  b.outputIndex,
		Item:   completedItem,
	})
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemCompleted, Data: completedData}}), nil
}

// functionCallStatus reports the terminal status for a streamed function
// call's accumulated arguments. With eager_input_streaming the upstream no
// longer validates tool-input JSON, so a truncated (max_tokens) or malformed
// argument string reaches us verbatim; surface it as StatusIncomplete rather
// than masquerade as a runnable call. Empty args stay completed — a no-arg
// tool may stream zero input_json_delta frames.
func functionCallStatus(args string) v1.Status {
	if args == "" || json.Valid([]byte(args)) {
		return v1.StatusCompleted
	}
	return v1.StatusIncomplete
}
