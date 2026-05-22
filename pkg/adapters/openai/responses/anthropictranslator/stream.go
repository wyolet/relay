package anthropictranslator

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	pkgopenai "github.com/wyolet/relay/pkg/adapters/openai"
)

// emittedItem tracks state for one output item opened during streaming.
type emittedItem struct {
	itemType    string // "text", "tool_use", "thinking"
	outputIndex int
	itemID      string
	// text accumulation
	textBuf strings.Builder
	// tool_use accumulation
	callID   string
	toolName string
	argsBuf  strings.Builder
	// thinking accumulation
	thinkingBuf strings.Builder
}

// Stream is a stateful per-stream SSE translator.
// Create one per upstream Anthropic SSE stream; call Translate for each chunk.
//
// Each Anthropic text block becomes its own Responses message item
// (one output_text content part each). Multiple text blocks → multiple
// message items in Responses output. This is the simplest mapping and
// avoids the need to decide on a merge boundary.
//
// server_tool_use blocks are dropped (not modeled in v1 canonical output).
// redacted_thinking blocks are dropped.
type Stream struct {
	req *pkgopenai.ResponsesRequest // original request, echoed into completed event

	// response-level state set from message_start
	responseID  string
	model       string
	created     int64
	inputTokens int

	// current block
	currentBlock *emittedItem

	// all blocks finalised so far (for final response reconstruction)
	done []*emittedItem

	// running usage from message_delta
	outputTokens int
	cachedTokens int

	// stop_reason from message_delta
	stopReason string
}

// NewStream returns a new stateful Anthropic → Responses SSE translator.
// req is the original Responses API request; it is echoed into the
// response.completed event. Pass nil to omit echo fields (tests).
func NewStream(req *pkgopenai.ResponsesRequest) *Stream {
	return &Stream{req: req}
}

// Translate converts one Anthropic SSE chunk (raw bytes of one SSE frame,
// including optional "event:" line and "data:" line) to zero or more Responses
// SSE frames.
func (s *Stream) Translate(chunk []byte) ([]pkgopenai.ResponsesSSEFrame, error) {
	event, data, ok := pkgopenai.ParseResponsesSSEChunk(chunk)
	if !ok || len(data) == 0 {
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
	case "ping", "":
		return nil, nil
	case "error":
		return s.handleError(data)
	default:
		return nil, nil
	}
}

// ---- event handlers ----

func (s *Stream) handleMessageStart(data []byte) ([]pkgopenai.ResponsesSSEFrame, error) {
	var ms struct {
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				CacheRead    int `json:"cache_read_input_tokens"`
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

	// Emit response.created + response.in_progress
	snap := s.buildResponseSnapshot(pkgopenai.ResponsesStatusInProgress, "", "")
	createdFrame, err := marshalFrame(pkgopenai.ResponsesEventCreated, pkgopenai.ResponsesCreatedEvent{Response: snap})
	if err != nil {
		return nil, err
	}
	inProgFrame, err := marshalFrame(pkgopenai.ResponsesEventInProgress, pkgopenai.ResponsesInProgressEvent{Response: snap})
	if err != nil {
		return nil, err
	}
	return []pkgopenai.ResponsesSSEFrame{createdFrame, inProgFrame}, nil
}

func (s *Stream) handleContentBlockStart(data []byte) ([]pkgopenai.ResponsesSSEFrame, error) {
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

	// server_tool_use and redacted_thinking are dropped.
	if blockType == "server_tool_use" || blockType == "redacted_thinking" {
		return nil, nil
	}

	outputIndex := len(s.done)
	if s.currentBlock != nil {
		// Shouldn't happen (content_block_stop should precede), but guard anyway.
		outputIndex = s.currentBlock.outputIndex + 1
	}

	item := &emittedItem{
		itemType:    blockType,
		outputIndex: outputIndex,
		itemID:      fmt.Sprintf("%s_%d", blockType[:1], outputIndex),
	}

	switch blockType {
	case "text":
		s.currentBlock = item
		// Emit output_item.added for a new message item, then content_part.added.
		msgItem := &pkgopenai.ResponsesMessage{
			ID:     item.itemID,
			Status: pkgopenai.ResponsesStatusInProgress,
			Role:   pkgopenai.ResponsesRoleAssistant,
		}
		itemAddedFrame, err := marshalFrame(pkgopenai.ResponsesEventOutputItemAdded, pkgopenai.ResponsesItemAddedEvent{
			OutputIndex: outputIndex,
			Item:        msgItem,
		})
		if err != nil {
			return nil, err
		}
		partAddedFrame, err := marshalFrame(pkgopenai.ResponsesEventContentPartAdded, pkgopenai.ResponsesContentPartAddedEvent{
			ItemID:       item.itemID,
			OutputIndex:  outputIndex,
			ContentIndex: 0,
			Part:         &pkgopenai.ResponsesOutputTextPart{Text: ""},
		})
		if err != nil {
			return nil, err
		}
		return []pkgopenai.ResponsesSSEFrame{itemAddedFrame, partAddedFrame}, nil

	case "tool_use":
		item.callID = cbs.ContentBlock.ID
		item.toolName = cbs.ContentBlock.Name
		s.currentBlock = item
		fcItem := &pkgopenai.ResponsesFunctionCall{
			ID:     item.itemID,
			CallID: item.callID,
			Name:   item.toolName,
			Status: pkgopenai.ResponsesStatusInProgress,
		}
		f, err := marshalFrame(pkgopenai.ResponsesEventOutputItemAdded, pkgopenai.ResponsesItemAddedEvent{
			OutputIndex: outputIndex,
			Item:        fcItem,
		})
		if err != nil {
			return nil, err
		}
		return []pkgopenai.ResponsesSSEFrame{f}, nil

	case "thinking":
		s.currentBlock = item
		rItem := &pkgopenai.ResponsesReasoning{
			ID:     item.itemID,
			Status: pkgopenai.ResponsesStatusInProgress,
		}
		itemAddedFrame, err := marshalFrame(pkgopenai.ResponsesEventOutputItemAdded, pkgopenai.ResponsesItemAddedEvent{
			OutputIndex: outputIndex,
			Item:        rItem,
		})
		if err != nil {
			return nil, err
		}
		// Reasoning text uses content_index 0.
		partAddedFrame, err := marshalFrame(pkgopenai.ResponsesEventContentPartAdded, pkgopenai.ResponsesContentPartAddedEvent{
			ItemID:       item.itemID,
			OutputIndex:  outputIndex,
			ContentIndex: 0,
			Part:         &pkgopenai.ResponsesOutputTextPart{Text: ""},
		})
		if err != nil {
			return nil, err
		}
		return []pkgopenai.ResponsesSSEFrame{itemAddedFrame, partAddedFrame}, nil

	default:
		// Unknown block type — skip silently.
		return nil, nil
	}
}

func (s *Stream) handleContentBlockDelta(data []byte) ([]pkgopenai.ResponsesSSEFrame, error) {
	if s.currentBlock == nil {
		return nil, nil
	}

	var cbd struct {
		Index int `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text,omitempty"`
			PartialJSON string `json:"partial_json,omitempty"`
			Thinking    string `json:"thinking,omitempty"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(data, &cbd); err != nil {
		return nil, fmt.Errorf("anthropic stream: content_block_delta: %w", err)
	}

	item := s.currentBlock

	switch cbd.Delta.Type {
	case "text_delta":
		item.textBuf.WriteString(cbd.Delta.Text)
		f, err := marshalFrame(pkgopenai.ResponsesEventOutputTextDelta, pkgopenai.ResponsesOutputTextDeltaEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Delta:        cbd.Delta.Text,
		})
		if err != nil {
			return nil, err
		}
		return []pkgopenai.ResponsesSSEFrame{f}, nil

	case "input_json_delta":
		item.argsBuf.WriteString(cbd.Delta.PartialJSON)
		f, err := marshalFrame(pkgopenai.ResponsesEventFunctionCallArgumentsDelta, pkgopenai.ResponsesFunctionCallArgumentsDeltaEvent{
			ItemID:      item.itemID,
			OutputIndex: item.outputIndex,
			CallID:      item.callID,
			Delta:       cbd.Delta.PartialJSON,
		})
		if err != nil {
			return nil, err
		}
		return []pkgopenai.ResponsesSSEFrame{f}, nil

	case "thinking_delta":
		item.thinkingBuf.WriteString(cbd.Delta.Thinking)
		f, err := marshalFrame(pkgopenai.ResponsesEventReasoningTextDelta, pkgopenai.ResponsesReasoningTextDeltaEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Delta:        cbd.Delta.Thinking,
		})
		if err != nil {
			return nil, err
		}
		return []pkgopenai.ResponsesSSEFrame{f}, nil
	}

	return nil, nil
}

func (s *Stream) handleContentBlockStop(_ []byte) ([]pkgopenai.ResponsesSSEFrame, error) {
	if s.currentBlock == nil {
		return nil, nil
	}

	item := s.currentBlock
	s.currentBlock = nil
	s.done = append(s.done, item)

	var frames []pkgopenai.ResponsesSSEFrame

	switch item.itemType {
	case "text":
		textDone, err := marshalFrame(pkgopenai.ResponsesEventOutputTextDone, pkgopenai.ResponsesOutputTextDoneEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Text:         item.textBuf.String(),
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, textDone)

		partDone, err := marshalFrame(pkgopenai.ResponsesEventContentPartDone, pkgopenai.ResponsesContentPartDoneEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Part:         &pkgopenai.ResponsesOutputTextPart{Text: item.textBuf.String()},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, partDone)

		msgDone := &pkgopenai.ResponsesMessage{
			ID:      item.itemID,
			Status:  pkgopenai.ResponsesStatusCompleted,
			Role:    pkgopenai.ResponsesRoleAssistant,
			Content: []pkgopenai.ResponsesPart{&pkgopenai.ResponsesOutputTextPart{Text: item.textBuf.String()}},
		}
		itemDone, err := marshalFrame(pkgopenai.ResponsesEventOutputItemDone, pkgopenai.ResponsesOutputItemDoneEvent{
			OutputIndex: item.outputIndex,
			Item:        msgDone,
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, itemDone)

	case "tool_use":
		argsDone, err := marshalFrame(pkgopenai.ResponsesEventFunctionCallArgumentsDone, pkgopenai.ResponsesFunctionCallArgumentsDoneEvent{
			ItemID:      item.itemID,
			OutputIndex: item.outputIndex,
			CallID:      item.callID,
			Arguments:   item.argsBuf.String(),
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, argsDone)

		fcDone := &pkgopenai.ResponsesFunctionCall{
			ID:        item.itemID,
			CallID:    item.callID,
			Name:      item.toolName,
			Arguments: item.argsBuf.String(),
			Status:    pkgopenai.ResponsesStatusCompleted,
		}
		itemDone, err := marshalFrame(pkgopenai.ResponsesEventOutputItemDone, pkgopenai.ResponsesOutputItemDoneEvent{
			OutputIndex: item.outputIndex,
			Item:        fcDone,
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, itemDone)

	case "thinking":
		thinkingDone, err := marshalFrame(pkgopenai.ResponsesEventReasoningTextDone, pkgopenai.ResponsesReasoningTextDoneEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Text:         item.thinkingBuf.String(),
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, thinkingDone)

		partDone, err := marshalFrame(pkgopenai.ResponsesEventContentPartDone, pkgopenai.ResponsesContentPartDoneEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Part:         &pkgopenai.ResponsesOutputTextPart{Text: item.thinkingBuf.String()},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, partDone)

		rDone := &pkgopenai.ResponsesReasoning{
			ID:     item.itemID,
			Status: pkgopenai.ResponsesStatusCompleted,
			Summary: []pkgopenai.ResponsesSummaryText{
				{Text: item.thinkingBuf.String()},
			},
		}
		itemDone, err := marshalFrame(pkgopenai.ResponsesEventOutputItemDone, pkgopenai.ResponsesOutputItemDoneEvent{
			OutputIndex: item.outputIndex,
			Item:        rDone,
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, itemDone)
	}

	return frames, nil
}

func (s *Stream) handleMessageDelta(data []byte) ([]pkgopenai.ResponsesSSEFrame, error) {
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
	s.stopReason = md.Delta.StopReason
	s.outputTokens = md.Usage.OutputTokens
	// No events emitted — state updated for message_stop.
	return nil, nil
}

func (s *Stream) handleMessageStop() ([]pkgopenai.ResponsesSSEFrame, error) {
	status, finish, incomplete := mapStopReason(s.stopReason)

	// Reconstruct final output from done items.
	output := s.buildOutputItems()

	// input_tokens_details and output_tokens_details are always set (spec required).
	total := s.inputTokens + s.outputTokens
	u := &pkgopenai.ResponsesUsage{
		InputTokens:         s.inputTokens,
		OutputTokens:        s.outputTokens,
		TotalTokens:         total,
		InputTokensDetails:  pkgopenai.ResponsesInputDeets{CachedTokens: s.cachedTokens},
		OutputTokensDetails: pkgopenai.ResponsesOutputDeets{},
	}

	finalResp := &pkgopenai.ResponsesResponse{
		ID:           s.responseID,
		Object:       "response",
		CreatedAt:    s.created,
		Model:        s.model,
		Status:       status,
		FinishReason: finish,
		Output:       output,
		Usage:        u,
	}
	if incomplete != "" {
		finalResp.IncompleteDetails = &pkgopenai.ResponsesIncompleteDetails{Reason: incomplete}
	}

	pkgopenai.ResponsesEchoRequest(finalResp, s.req)

	var eventName string
	switch status {
	case pkgopenai.ResponsesStatusCompleted:
		eventName = pkgopenai.ResponsesEventCompleted
	case pkgopenai.ResponsesStatusIncomplete:
		eventName = pkgopenai.ResponsesEventIncomplete
	default:
		eventName = pkgopenai.ResponsesEventCompleted
	}

	f, err := marshalFrame(eventName, pkgopenai.ResponsesCompletedEvent{Response: finalResp})
	if err != nil {
		return nil, err
	}
	return []pkgopenai.ResponsesSSEFrame{f}, nil
}

func (s *Stream) handleError(data []byte) ([]pkgopenai.ResponsesSSEFrame, error) {
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
	f, err := marshalFrame(pkgopenai.ResponsesEventError, pkgopenai.ResponsesErrorEvent{
		Code:    e.Error.Type,
		Message: msg,
	})
	if err != nil {
		return nil, err
	}
	return []pkgopenai.ResponsesSSEFrame{f}, nil
}

// ---- helpers ----

// buildResponseSnapshot builds a minimal Response for in-progress events.
func (s *Stream) buildResponseSnapshot(status pkgopenai.ResponsesStatus, finish pkgopenai.ResponsesFinishReason, incomplete string) *pkgopenai.ResponsesResponse {
	r := &pkgopenai.ResponsesResponse{
		ID:           s.responseID,
		Object:       "response",
		CreatedAt:    s.created,
		Model:        s.model,
		Status:       status,
		FinishReason: finish,
		Output:       []pkgopenai.ResponsesItem{},
	}
	if incomplete != "" {
		r.IncompleteDetails = &pkgopenai.ResponsesIncompleteDetails{Reason: incomplete}
	}
	return r
}

// buildOutputItems reconstructs the final []Item from finalised emittedItems.
func (s *Stream) buildOutputItems() []pkgopenai.ResponsesItem {
	items := make([]pkgopenai.ResponsesItem, 0, len(s.done))
	for _, d := range s.done {
		switch d.itemType {
		case "text":
			msg := &pkgopenai.ResponsesMessage{
				ID:     d.itemID,
				Status: pkgopenai.ResponsesStatusCompleted,
				Role:   pkgopenai.ResponsesRoleAssistant,
				Content: []pkgopenai.ResponsesPart{
					&pkgopenai.ResponsesOutputTextPart{Text: d.textBuf.String()},
				},
			}
			items = append(items, msg)
		case "tool_use":
			items = append(items, &pkgopenai.ResponsesFunctionCall{
				ID:        d.itemID,
				CallID:    d.callID,
				Name:      d.toolName,
				Arguments: d.argsBuf.String(),
				Status:    pkgopenai.ResponsesStatusCompleted,
			})
		case "thinking":
			items = append(items, &pkgopenai.ResponsesReasoning{
				ID:     d.itemID,
				Status: pkgopenai.ResponsesStatusCompleted,
				Summary: []pkgopenai.ResponsesSummaryText{
					{Text: d.thinkingBuf.String()},
				},
			})
		}
	}
	return items
}

// marshalFrame builds a Responses responses.SSEFrame by marshaling the data payload.
func marshalFrame(event string, data any) (pkgopenai.ResponsesSSEFrame, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return pkgopenai.ResponsesSSEFrame{}, fmt.Errorf("marshalFrame %s: %w", event, err)
	}
	return pkgopenai.ResponsesSSEFrame{Event: event, Data: b}, nil
}

