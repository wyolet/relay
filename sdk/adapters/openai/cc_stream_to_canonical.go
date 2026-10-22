package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wyolet/relay/sdk/internal/sse"
	"github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// NewToCanonicalStream returns a stateful per-stream function that converts one
// CC SSE chunk into one or more canonical SSE chunks.
func (CCTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &ccToCanonicalStream{}
	return s.translate
}

type ccStreamItemKind int

const (
	ccStreamKindMessage ccStreamItemKind = iota
	ccStreamKindToolCall
	ccStreamKindReasoning
)

// ccStreamItem is only ever held by pointer: a strings.Builder panics when
// written to after being copied.
type ccStreamItem struct {
	kind        ccStreamItemKind
	outputIndex int
	itemID      string
	callID      string
	name        string
	textBuf     strings.Builder
	argsBuf     []byte
	// reasoningField records the wire field name (reasoning|reasoning_content)
	// for reasoning items, preserved into the canonical item's provider_data.
	reasoningField string
}

// ccToCanonicalStream is a stateful CC SSE → canonical SSE translator.
type ccToCanonicalStream struct {
	responseID       string
	model            string
	created          int64
	nextIndex        int
	msgItem          *ccStreamItem
	reasoningItem    *ccStreamItem
	toolItems        map[int]*ccStreamItem
	lastUsage        *Usage
	serviceTier      string
	lifecycleEmitted bool
	status           v1.Status
	finishReason     v1.FinishReason
	incomplete       *v1.IncompleteDetails
	errorEmitted     bool
	// refused records a refusal delta: CC terminates a refusal with "stop",
	// so handleDone lifts the finish_reason from it (rule 9).
	refused bool
	// finishSeen separates "no finish_reason arrived" (defaults to stop) from
	// an unknown one, which maps to other on an incomplete status.
	finishSeen bool
}

// ccStreamChunk is everything translateEvent reads from a chunk: the
// ChatStreamChunk fields, a top-level error and the delta's reasoning text.
// Choices and Delta shadow the embedded ones, so every ChatStreamChunk field
// still decodes and a chunk that decodes here decodes as ChatStreamChunk too.
type ccStreamChunk struct {
	ChatStreamChunk
	Choices []ccStreamChoice `json:"choices"`
	Error   json.RawMessage  `json:"error"`
}

type ccStreamChoice struct {
	StreamChoice
	Delta ccStreamDelta `json:"delta"`
}

// ccStreamDelta carries reasoning under both names it streams as:
// reasoning_content (OpenAI-compatible) and reasoning (Ollama).
type ccStreamDelta struct {
	StreamDelta
	ReasoningContent string `json:"reasoning_content"`
	Reasoning        string `json:"reasoning"`
}

// reasoningText returns the reasoning text and the field that carried it,
// preferring reasoning_content.
func (d *ccStreamDelta) reasoningText() (text, field string) {
	if d.ReasoningContent != "" {
		return d.ReasoningContent, ccReasoningFieldStd
	}
	if d.Reasoning != "" {
		return d.Reasoning, ccReasoningFieldOllama
	}
	return "", ""
}

// decodeCCStreamChunk decodes data in one pass. When that fails it decodes the
// parts on their own: the error field alone, so an error frame survives faults
// elsewhere, then ChatStreamChunk, whose error is the one returned. Reasoning
// stays empty then: a mistyped reasoning field is the only fault
// ChatStreamChunk tolerates, and it voids the reasoning of every choice.
func decodeCCStreamChunk(data []byte) (ccStreamChunk, error) {
	var c ccStreamChunk
	if json.Unmarshal(data, &c) == nil {
		return c, nil
	}
	c = ccStreamChunk{}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &probe) == nil {
		c.Error = probe.Error
	}
	if err := json.Unmarshal(data, &c.ChatStreamChunk); err != nil {
		return c, err
	}
	for _, ch := range c.ChatStreamChunk.Choices {
		c.Choices = append(c.Choices, ccStreamChoice{StreamChoice: ch, Delta: ccStreamDelta{StreamDelta: ch.Delta}})
	}
	return c, nil
}

func ccStreamErrorFrame(errField json.RawMessage, data []byte) (v1.SSEFrame, bool) {
	errRaw := bytes.TrimSpace(errField)
	if len(errRaw) == 0 || bytes.Equal(errRaw, []byte("null")) {
		return v1.SSEFrame{}, false
	}

	var ccErr struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	}
	_ = json.Unmarshal(errRaw, &ccErr)

	code := ccErr.Type
	if code == "" && len(ccErr.Code) > 0 {
		var codeString string
		if err := json.Unmarshal(ccErr.Code, &codeString); err == nil {
			code = codeString
		} else {
			code = string(bytes.TrimSpace(ccErr.Code))
		}
	}
	msg := ccErr.Message
	if msg == "" {
		msg = string(data)
	}
	errData, _ := json.Marshal(v1.ErrorEvent{
		Code:    code,
		Message: msg,
	})
	return v1.SSEFrame{Event: v1.EventError, Data: errData}, true
}

func (s *ccToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	var out []byte
	sc := sse.NewScanner(chunk)
	for sc.Next() {
		b, err := s.translateEvent(sc.Data())
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = b
		} else {
			out = append(out, b...)
		}
	}
	return out, nil
}

func (s *ccToCanonicalStream) translateEvent(data []byte) ([]byte, error) {
	if s.errorEmitted {
		return nil, nil
	}

	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return s.handleDone()
	}

	ccChunk, err := decodeCCStreamChunk(data)
	if errFrame, ok := ccStreamErrorFrame(ccChunk.Error, data); ok {
		s.errorEmitted = true
		return marshalCanonicalFrames([]v1.SSEFrame{errFrame}), nil
	}
	if err != nil {
		return nil, fmt.Errorf("cc stream: parse chunk: %w", err)
	}

	if ccChunk.Usage != nil {
		s.lastUsage = ccChunk.Usage
	}
	if ccChunk.ServiceTier != "" {
		s.serviceTier = ccChunk.ServiceTier
	}

	var frames []v1.SSEFrame

	if !s.lifecycleEmitted {
		s.responseID = ccChunk.ID
		s.model = ccChunk.Model
		s.created = ccChunk.Created
		if s.created == 0 {
			s.created = time.Now().Unix()
		}
		if s.responseID == "" {
			s.responseID = fmt.Sprintf("resp_%d", s.created)
		}
		if s.toolItems == nil {
			s.toolItems = make(map[int]*ccStreamItem)
		}
		// Emit generation.created
		createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
			ID:    s.responseID,
			Model: s.model,
		})
		frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCreated, Data: createdData})
		s.lifecycleEmitted = true
	}

	if len(ccChunk.Choices) == 0 {
		return marshalCanonicalFrames(frames), nil
	}

	ch := ccChunk.Choices[0]
	delta := ch.Delta
	// canonical: per-chunk logprobs dropped — canonical stream events have no
	// extensions slot; the buffered path carries them in Response.Extensions.

	// finish_reason arrives on the terminal chunk (separate from deltas); capture
	// it so handleDone emits the real reason instead of a hardcoded "stop".
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		s.status, s.finishReason, s.incomplete = ccFinishReasonToCanonical(*ch.FinishReason)
		s.finishSeen = true
	}

	// Reasoning text (Ollama "reasoning" or o-series "reasoning_content").
	if rc, field := delta.reasoningText(); rc != "" {
		rf, err := s.handleReasoningDelta(rc, field)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Text content.
	if delta.Content != nil && *delta.Content != "" {
		rf, err := s.handleTextDelta(*delta.Content)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Tool calls.
	for _, tc := range delta.ToolCalls {
		rf, err := s.handleToolCallDelta(tc)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	// Refusal content.
	if delta.Refusal != nil && *delta.Refusal != "" {
		// Refusal text streams as a normal text delta; handleDone turns the
		// terminal finish_reason into refusal.
		s.refused = true
		rf, err := s.handleTextDelta(*delta.Refusal)
		if err != nil {
			return nil, err
		}
		frames = append(frames, rf...)
	}

	return marshalCanonicalFrames(frames), nil
}

func (s *ccToCanonicalStream) handleDone() ([]byte, error) {
	var frames []v1.SSEFrame

	// Close open reasoning item.
	if s.reasoningItem != nil {
		f := s.closeReasoningItem(s.reasoningItem)
		frames = append(frames, f...)
		s.reasoningItem = nil
	}

	// Close open message item.
	if s.msgItem != nil {
		f := s.closeMsgItem(s.msgItem)
		frames = append(frames, f...)
		s.msgItem = nil
	}

	// Close open tool call items.
	if len(s.toolItems) > 0 {
		type toolItemEntry struct {
			key  int
			item *ccStreamItem
		}
		toolItems := make([]toolItemEntry, 0, len(s.toolItems))
		for key, ti := range s.toolItems {
			toolItems = append(toolItems, toolItemEntry{key: key, item: ti})
		}
		sort.Slice(toolItems, func(i, j int) bool {
			if toolItems[i].item.outputIndex == toolItems[j].item.outputIndex {
				return toolItems[i].key < toolItems[j].key
			}
			return toolItems[i].item.outputIndex < toolItems[j].item.outputIndex
		})
		for _, entry := range toolItems {
			f := s.closeToolItem(entry.item)
			frames = append(frames, f...)
			delete(s.toolItems, entry.key)
		}
	}

	// generation.completed
	var u usage.Tokens
	if s.lastUsage != nil {
		u = ccUsageToCanonical(s.lastUsage)
	}
	status, finish, incomplete := s.status, s.finishReason, s.incomplete
	if !s.finishSeen {
		status, finish, incomplete = v1.StatusCompleted, v1.FinishReasonStop, nil
	}
	if s.refused && finish == v1.FinishReasonStop {
		finish = v1.FinishReasonRefusal
	}
	completedData, _ := json.Marshal(v1.GenerationCompletedEvent{
		ID:                s.responseID,
		Status:            status,
		FinishReason:      finish,
		Usage:             u,
		IncompleteDetails: incomplete,
		ServiceTier:       s.serviceTier,
	})
	frames = append(frames, v1.SSEFrame{Event: v1.EventGenerationCompleted, Data: completedData})

	return marshalCanonicalFrames(frames), nil
}
