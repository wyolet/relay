package anthropictranslator

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wyolet/relay/pkg/adapters/openai/responses"
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
	req *responses.Request // original request, echoed into completed event

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
func NewStream(req *responses.Request) *Stream {
	return &Stream{req: req}
}

// Translate converts one Anthropic SSE chunk (raw bytes of one SSE frame,
// including optional "event:" line and "data:" line) to zero or more Responses
// SSE frames.
func (s *Stream) Translate(chunk []byte) ([]responses.SSEFrame, error) {
	event, data, ok := responses.ParseSSEChunk(chunk)
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

func (s *Stream) handleMessageStart(data []byte) ([]responses.SSEFrame, error) {
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
	snap := s.buildResponseSnapshot(responses.StatusInProgress, "", "")
	createdFrame, err := marshalFrame(responses.EventCreated, responses.CreatedEvent{Response: snap})
	if err != nil {
		return nil, err
	}
	inProgFrame, err := marshalFrame(responses.EventInProgress, responses.InProgressEvent{Response: snap})
	if err != nil {
		return nil, err
	}
	return []responses.SSEFrame{createdFrame, inProgFrame}, nil
}

func (s *Stream) handleContentBlockStart(data []byte) ([]responses.SSEFrame, error) {
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
		msgItem := &responses.Message{
			ID:     item.itemID,
			Status: responses.StatusInProgress,
			Role:   responses.RoleAssistant,
		}
		itemAddedFrame, err := marshalFrame(responses.EventOutputItemAdded, responses.ItemAddedEvent{
			OutputIndex: outputIndex,
			Item:        msgItem,
		})
		if err != nil {
			return nil, err
		}
		partAddedFrame, err := marshalFrame(responses.EventContentPartAdded, responses.ContentPartAddedEvent{
			ItemID:       item.itemID,
			OutputIndex:  outputIndex,
			ContentIndex: 0,
			Part:         &responses.OutputTextPart{Text: ""},
		})
		if err != nil {
			return nil, err
		}
		return []responses.SSEFrame{itemAddedFrame, partAddedFrame}, nil

	case "tool_use":
		item.callID = cbs.ContentBlock.ID
		item.toolName = cbs.ContentBlock.Name
		s.currentBlock = item
		fcItem := &responses.FunctionCall{
			ID:     item.itemID,
			CallID: item.callID,
			Name:   item.toolName,
			Status: responses.StatusInProgress,
		}
		f, err := marshalFrame(responses.EventOutputItemAdded, responses.ItemAddedEvent{
			OutputIndex: outputIndex,
			Item:        fcItem,
		})
		if err != nil {
			return nil, err
		}
		return []responses.SSEFrame{f}, nil

	case "thinking":
		s.currentBlock = item
		rItem := &responses.Reasoning{
			ID:     item.itemID,
			Status: responses.StatusInProgress,
		}
		itemAddedFrame, err := marshalFrame(responses.EventOutputItemAdded, responses.ItemAddedEvent{
			OutputIndex: outputIndex,
			Item:        rItem,
		})
		if err != nil {
			return nil, err
		}
		// Reasoning text uses content_index 0.
		partAddedFrame, err := marshalFrame(responses.EventContentPartAdded, responses.ContentPartAddedEvent{
			ItemID:       item.itemID,
			OutputIndex:  outputIndex,
			ContentIndex: 0,
			Part:         &responses.OutputTextPart{Text: ""},
		})
		if err != nil {
			return nil, err
		}
		return []responses.SSEFrame{itemAddedFrame, partAddedFrame}, nil

	default:
		// Unknown block type — skip silently.
		return nil, nil
	}
}

func (s *Stream) handleContentBlockDelta(data []byte) ([]responses.SSEFrame, error) {
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
		f, err := marshalFrame(responses.EventOutputTextDelta, responses.OutputTextDeltaEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Delta:        cbd.Delta.Text,
		})
		if err != nil {
			return nil, err
		}
		return []responses.SSEFrame{f}, nil

	case "input_json_delta":
		item.argsBuf.WriteString(cbd.Delta.PartialJSON)
		f, err := marshalFrame(responses.EventFunctionCallArgumentsDelta, responses.FunctionCallArgumentsDeltaEvent{
			ItemID:      item.itemID,
			OutputIndex: item.outputIndex,
			CallID:      item.callID,
			Delta:       cbd.Delta.PartialJSON,
		})
		if err != nil {
			return nil, err
		}
		return []responses.SSEFrame{f}, nil

	case "thinking_delta":
		item.thinkingBuf.WriteString(cbd.Delta.Thinking)
		f, err := marshalFrame(responses.EventReasoningTextDelta, responses.ReasoningTextDeltaEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Delta:        cbd.Delta.Thinking,
		})
		if err != nil {
			return nil, err
		}
		return []responses.SSEFrame{f}, nil
	}

	return nil, nil
}

func (s *Stream) handleContentBlockStop(_ []byte) ([]responses.SSEFrame, error) {
	if s.currentBlock == nil {
		return nil, nil
	}

	item := s.currentBlock
	s.currentBlock = nil
	s.done = append(s.done, item)

	var frames []responses.SSEFrame

	switch item.itemType {
	case "text":
		textDone, err := marshalFrame(responses.EventOutputTextDone, responses.OutputTextDoneEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Text:         item.textBuf.String(),
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, textDone)

		partDone, err := marshalFrame(responses.EventContentPartDone, responses.ContentPartDoneEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Part:         &responses.OutputTextPart{Text: item.textBuf.String()},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, partDone)

		msgDone := &responses.Message{
			ID:      item.itemID,
			Status:  responses.StatusCompleted,
			Role:    responses.RoleAssistant,
			Content: []responses.Part{&responses.OutputTextPart{Text: item.textBuf.String()}},
		}
		itemDone, err := marshalFrame(responses.EventOutputItemDone, responses.OutputItemDoneEvent{
			OutputIndex: item.outputIndex,
			Item:        msgDone,
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, itemDone)

	case "tool_use":
		argsDone, err := marshalFrame(responses.EventFunctionCallArgumentsDone, responses.FunctionCallArgumentsDoneEvent{
			ItemID:      item.itemID,
			OutputIndex: item.outputIndex,
			CallID:      item.callID,
			Arguments:   item.argsBuf.String(),
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, argsDone)

		fcDone := &responses.FunctionCall{
			ID:        item.itemID,
			CallID:    item.callID,
			Name:      item.toolName,
			Arguments: item.argsBuf.String(),
			Status:    responses.StatusCompleted,
		}
		itemDone, err := marshalFrame(responses.EventOutputItemDone, responses.OutputItemDoneEvent{
			OutputIndex: item.outputIndex,
			Item:        fcDone,
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, itemDone)

	case "thinking":
		thinkingDone, err := marshalFrame(responses.EventReasoningTextDone, responses.ReasoningTextDoneEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Text:         item.thinkingBuf.String(),
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, thinkingDone)

		partDone, err := marshalFrame(responses.EventContentPartDone, responses.ContentPartDoneEvent{
			ItemID:       item.itemID,
			OutputIndex:  item.outputIndex,
			ContentIndex: 0,
			Part:         &responses.OutputTextPart{Text: item.thinkingBuf.String()},
		})
		if err != nil {
			return nil, err
		}
		frames = append(frames, partDone)

		rDone := &responses.Reasoning{
			ID:     item.itemID,
			Status: responses.StatusCompleted,
			Summary: []responses.SummaryText{
				{Text: item.thinkingBuf.String()},
			},
		}
		itemDone, err := marshalFrame(responses.EventOutputItemDone, responses.OutputItemDoneEvent{
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

func (s *Stream) handleMessageDelta(data []byte) ([]responses.SSEFrame, error) {
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

func (s *Stream) handleMessageStop() ([]responses.SSEFrame, error) {
	status, finish, incomplete := mapStopReason(s.stopReason)

	// Reconstruct final output from done items.
	output := s.buildOutputItems()

	// input_tokens_details and output_tokens_details are always set (spec required).
	total := s.inputTokens + s.outputTokens
	u := &responses.Usage{
		InputTokens:         s.inputTokens,
		OutputTokens:        s.outputTokens,
		TotalTokens:         total,
		InputTokensDetails:  responses.InputDeets{CachedTokens: s.cachedTokens},
		OutputTokensDetails: responses.OutputDeets{},
	}

	finalResp := &responses.Response{
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
		finalResp.IncompleteDetails = &responses.IncompleteDetails{Reason: incomplete}
	}

	responses.EchoRequest(finalResp, s.req)

	var eventName string
	switch status {
	case responses.StatusCompleted:
		eventName = responses.EventCompleted
	case responses.StatusIncomplete:
		eventName = responses.EventIncomplete
	default:
		eventName = responses.EventCompleted
	}

	f, err := marshalFrame(eventName, responses.CompletedEvent{Response: finalResp})
	if err != nil {
		return nil, err
	}
	return []responses.SSEFrame{f}, nil
}

func (s *Stream) handleError(data []byte) ([]responses.SSEFrame, error) {
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
	f, err := marshalFrame(responses.EventError, responses.ErrorEvent{
		Code:    e.Error.Type,
		Message: msg,
	})
	if err != nil {
		return nil, err
	}
	return []responses.SSEFrame{f}, nil
}

// ---- helpers ----

// buildResponseSnapshot builds a minimal Response for in-progress events.
func (s *Stream) buildResponseSnapshot(status responses.Status, finish responses.FinishReason, incomplete string) *responses.Response {
	r := &responses.Response{
		ID:           s.responseID,
		Object:       "response",
		CreatedAt:    s.created,
		Model:        s.model,
		Status:       status,
		FinishReason: finish,
		Output:       []responses.Item{},
	}
	if incomplete != "" {
		r.IncompleteDetails = &responses.IncompleteDetails{Reason: incomplete}
	}
	return r
}

// buildOutputItems reconstructs the final []Item from finalised emittedItems.
func (s *Stream) buildOutputItems() []responses.Item {
	items := make([]responses.Item, 0, len(s.done))
	for _, d := range s.done {
		switch d.itemType {
		case "text":
			msg := &responses.Message{
				ID:     d.itemID,
				Status: responses.StatusCompleted,
				Role:   responses.RoleAssistant,
				Content: []responses.Part{
					&responses.OutputTextPart{Text: d.textBuf.String()},
				},
			}
			items = append(items, msg)
		case "tool_use":
			items = append(items, &responses.FunctionCall{
				ID:        d.itemID,
				CallID:    d.callID,
				Name:      d.toolName,
				Arguments: d.argsBuf.String(),
				Status:    responses.StatusCompleted,
			})
		case "thinking":
			items = append(items, &responses.Reasoning{
				ID:     d.itemID,
				Status: responses.StatusCompleted,
				Summary: []responses.SummaryText{
					{Text: d.thinkingBuf.String()},
				},
			})
		}
	}
	return items
}

// marshalFrame builds a Responses responses.SSEFrame by marshaling the data payload.
func marshalFrame(event string, data any) (responses.SSEFrame, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return responses.SSEFrame{}, fmt.Errorf("marshalFrame %s: %w", event, err)
	}
	return responses.SSEFrame{Event: event, Data: b}, nil
}

