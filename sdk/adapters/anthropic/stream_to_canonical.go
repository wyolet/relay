package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- NewToCanonicalStream ----

// NewToCanonicalStream returns a stateful per-stream function that converts
// Anthropic SSE chunks into canonical SSE chunks.
func (AnthropicTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &anthropicToCanonicalStream{}
	return s.translate
}

// ---- stream: Anthropic → canonical ----

type anthropicToCanonicalStream struct {
	responseID       string
	model            string
	created          int64
	nextIndex        int
	lifecycleEmitted bool
	currentBlock     *anthropicStreamBlock
	// accumulated usage from message_start + message_delta
	inputTokens         int
	outputTokens        int
	cachedTokens        int
	cacheCreationTokens int
	stopReason          string
	// structuredOutputSeen is set when a __relay_structured_output tool block
	// is completed. It prevents handleMessageDelta from overwriting the
	// already-corrected stop reason with "tool_use".
	structuredOutputSeen bool
}

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
	// structuredOutput marks a tool_use block that is the synthetic
	// __relay_structured_output tool. Its input_json_delta chunks are
	// accumulated in argsBuf but emitted as canonical text deltas so the
	// caller sees a normal message, not a function-call stream.
	structuredOutput bool
}

func (s *anthropicToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	switch event {
	case "message_start":
		return s.handleMessageStart(data)
	case "content_block_start":
		return s.handleContentBlockStart(data)
	case "content_block_delta":
		return s.handleContentBlockDelta(data)
	case "content_block_stop":
		return s.handleContentBlockStop(data)
	case "message_delta":
		return s.handleMessageDelta(data)
	case "message_stop":
		return s.handleMessageStop()
	case "error":
		return s.handleError(data)
	case "ping", "":
		return nil, nil
	default:
		return nil, nil
	}
}

func (s *anthropicToCanonicalStream) handleMessageStart(data []byte) ([]byte, error) {
	var ms struct {
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				InputTokens   int `json:"input_tokens"`
				CacheRead     int `json:"cache_read_input_tokens"`
				CacheCreation int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &ms); err != nil {
		return nil, fmt.Errorf("anthropic stream: message_start: %w", err)
	}
	s.responseID = ms.Message.ID
	s.model = ms.Message.Model
	s.created = time.Now().Unix()
	s.inputTokens = ms.Message.Usage.InputTokens
	s.cachedTokens = ms.Message.Usage.CacheRead
	s.cacheCreationTokens = ms.Message.Usage.CacheCreation

	if s.responseID == "" {
		s.responseID = fmt.Sprintf("resp_%d", s.created)
	}

	createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
		ID:    s.responseID,
		Model: s.model,
	})
	s.lifecycleEmitted = true
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventGenerationCreated, Data: createdData}}), nil
}

func (s *anthropicToCanonicalStream) handleContentBlockStart(data []byte) ([]byte, error) {
	var cbs struct {
		Index        int `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id,omitempty"`
			Name string `json:"name,omitempty"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal(data, &cbs); err != nil {
		return nil, fmt.Errorf("anthropic stream: content_block_start: %w", err)
	}

	blockType := cbs.ContentBlock.Type
	if blockType == "" {
		return nil, nil
	}

	// Drop server_tool_use and redacted_thinking.
	if blockType == "server_tool_use" || blockType == "redacted_thinking" {
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
		if b.toolName == structuredOutputToolName {
			// Treat as a text block — input_json_delta becomes canonical text deltas.
			b.structuredOutput = true
		}
	}

	s.currentBlock = b

	var itemType v1.ItemType
	switch blockType {
	case "text":
		itemType = v1.ItemTypeMessage
	case "tool_use":
		if b.structuredOutput {
			itemType = v1.ItemTypeMessage
		} else {
			itemType = v1.ItemTypeFunctionCall
		}
	case "thinking":
		itemType = v1.ItemTypeReasoning
	default:
		s.currentBlock = nil
		return nil, nil
	}

	startData, _ := json.Marshal(v1.ItemStartedEvent{
		ItemID:   b.itemID,
		ItemType: itemType,
		Name:     b.toolName,
		Index:    outputIndex,
	})
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
		if b.structuredOutput {
			// Emit as text delta — caller sees streaming JSON text, not function args.
			kind = v1.DeltaKindText
		} else {
			kind = v1.DeltaKindArguments
		}
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
		if b.structuredOutput {
			// Unwrap: emit as a completed Message with text content, finish_reason stop.
			completedItem = &v1.Message{
				ID:      b.itemID,
				Role:    v1.RoleAssistant,
				Status:  v1.StatusCompleted,
				Content: []v1.Part{&v1.OutputTextPart{Text: b.argsBuf.String()}},
			}
			s.structuredOutputSeen = true
		} else {
			completedItem = &v1.FunctionCall{
				ID:        b.itemID,
				CallID:    b.callID,
				Name:      b.toolName,
				Arguments: b.argsBuf.String(),
				Status:    functionCallStatus(b.argsBuf.String()),
			}
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

func (s *anthropicToCanonicalStream) handleMessageDelta(data []byte) ([]byte, error) {
	var md struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &md); err != nil {
		return nil, fmt.Errorf("anthropic stream: message_delta: %w", err)
	}
	// Don't let the wire "tool_use" stop_reason clobber the "end_turn" we
	// already committed when the structured-output tool block completed.
	if !s.structuredOutputSeen {
		s.stopReason = md.Delta.StopReason
	}
	s.outputTokens = md.Usage.OutputTokens
	return nil, nil
}

func (s *anthropicToCanonicalStream) handleMessageStop() ([]byte, error) {
	status, finish, incomplete := anthropicStopReasonToCanonical(s.stopReason)

	u := usage.Tokens{}
	if s.inputTokens > 0 {
		u["input"] = int64(s.inputTokens)
	}
	if s.outputTokens > 0 {
		u["output"] = int64(s.outputTokens)
	}
	if s.cachedTokens > 0 {
		u["cache_read"] = int64(s.cachedTokens)
	}
	if s.cacheCreationTokens > 0 {
		u["cache_creation"] = int64(s.cacheCreationTokens)
	}
	if len(u) == 0 {
		u = nil
	}

	gen := v1.GenerationCompletedEvent{
		ID:           s.responseID,
		Status:       status,
		FinishReason: finish,
		Usage:        u,
	}
	if incomplete != nil {
		// encode incomplete_details as extension — GenerationCompletedEvent
		// doesn't carry it directly, but we still want it signaled.
		// Map: if status=incomplete+pause_turn, use finish_reason placeholder.
		// For max_tokens: finish_reason=length is already set.
		// For pause_turn: no finish_reason; status alone signals it.
		_ = incomplete // status=incomplete already conveys this
	}

	completedData, _ := json.Marshal(gen)
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventGenerationCompleted, Data: completedData}}), nil
}

func (s *anthropicToCanonicalStream) handleError(data []byte) ([]byte, error) {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	msg := e.Error.Message
	if msg == "" {
		msg = string(data)
	}
	errData, _ := json.Marshal(v1.ErrorEvent{
		Code:    e.Error.Type,
		Message: msg,
	})
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventError, Data: errData}}), nil
}

// ---- shared helpers ----

func anthropicSSEBytes(event, data string) []byte {
	var b strings.Builder
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.WriteString(data)
	b.WriteString("\n\n")
	return []byte(b.String())
}

func marshalCanonFrames(frames []v1.SSEFrame) []byte {
	var buf []byte
	for _, f := range frames {
		buf = append(buf, f.Bytes()...)
	}
	return buf
}
