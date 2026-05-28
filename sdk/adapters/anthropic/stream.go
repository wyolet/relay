// Package anthropic — per-chunk SSE transform between Anthropic and OpenAI streaming shapes.
//
// Each exported transformer is stateful: it tracks accumulated tool-call state
// across chunks so callers can call TransformChunk once per SSE message.
package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// AnthropicToOpenAI transforms a single Anthropic SSE chunk (one data: line,
// potentially with an event: prefix) into zero or more OpenAI SSE chunks.
// The transformer is stateful — create one per stream.
type AnthropicToOpenAI struct {
	msgID   string
	model   string
	created int64
	// tool state: index → {id, name}
	toolIndex int // current active content_block index for tool_use
	tools     map[int]toolState
}

type toolState struct {
	id   string
	name string
	oaix int // openai tool_calls index
}

// TransformChunk converts one Anthropic SSE chunk to zero or more OpenAI SSE
// chunks. The returned slice may be nil (event is a no-op ping, etc.).
func (t *AnthropicToOpenAI) TransformChunk(chunk []byte) ([]byte, error) {
	event, data, ok := parseSSEChunk(chunk)
	if !ok || len(data) == 0 {
		return nil, nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		return sseBytes("", "[DONE]"), nil
	}

	switch event {
	case "message_start":
		var ms struct {
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal(data, &ms); err != nil {
			return nil, fmt.Errorf("anthropic stream: message_start: %w", err)
		}
		t.msgID = ms.Message.ID
		t.model = ms.Message.Model
		t.created = time.Now().Unix()
		t.tools = make(map[int]toolState)
		// Emit a role-only delta.
		role := "assistant"
		return t.chunk(streamDelta{Role: &role}, nil), nil

	case "content_block_start":
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
		if cbs.ContentBlock.Type == "tool_use" {
			oaix := len(t.tools)
			t.tools[cbs.Index] = toolState{id: cbs.ContentBlock.ID, name: cbs.ContentBlock.Name, oaix: oaix}
			t.toolIndex = cbs.Index
			// Emit tool_calls chunk with id+type+name.
			emptyArgs := ""
			return t.chunk(streamDelta{
				ToolCalls: []oaToolCallChunk{{
					Index: oaix,
					ID:    cbs.ContentBlock.ID,
					Type:  "function",
					Function: &oaToolFnChunk{
						Name:      cbs.ContentBlock.Name,
						Arguments: &emptyArgs,
					},
				}},
			}, nil), nil
		}
		// text block start: no output needed
		return nil, nil

	case "content_block_delta":
		var cbd struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text,omitempty"`
				PartialJSON string `json:"partial_json,omitempty"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(data, &cbd); err != nil {
			return nil, fmt.Errorf("anthropic stream: content_block_delta: %w", err)
		}
		switch cbd.Delta.Type {
		case "text_delta":
			return t.chunk(streamDelta{Content: &cbd.Delta.Text}, nil), nil
		case "input_json_delta":
			ts, ok := t.tools[cbd.Index]
			if !ok {
				return nil, nil
			}
			args := cbd.Delta.PartialJSON
			return t.chunk(streamDelta{
				ToolCalls: []oaToolCallChunk{{
					Index: ts.oaix,
					Function: &oaToolFnChunk{
						Arguments: &args,
					},
				}},
			}, nil), nil
		}
		return nil, nil

	case "content_block_stop":
		return nil, nil

	case "message_delta":
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
		finish := mapStopReason(md.Delta.StopReason)
		return t.chunkFinish(&finish, nil), nil

	case "message_stop":
		return sseBytes("", "[DONE]"), nil

	case "ping", "":
		return nil, nil

	default:
		return nil, nil
	}
}

func (t *AnthropicToOpenAI) chunk(delta streamDelta, finish *string) []byte {
	return t.chunkFinish(finish, &delta)
}

func (t *AnthropicToOpenAI) chunkFinish(finish *string, delta *streamDelta) []byte {
	sc := oaStreamChunk{
		ID:      t.msgID,
		Object:  "chat.completion.chunk",
		Created: t.created,
		Model:   t.model,
	}
	ch := oaStreamChoice{Index: 0, FinishReason: finish}
	if delta != nil {
		ch.Delta = *delta
	}
	sc.Choices = []oaStreamChoice{ch}
	b, _ := json.Marshal(sc)
	return sseBytes("", string(b))
}

// ---- OpenAI → Anthropic ----

// OpenAIToAnthropic transforms a single OpenAI SSE chunk into Anthropic SSE chunks.
type OpenAIToAnthropic struct {
	msgID       string
	model       string
	blockIndex  int
	toolBlocks  map[int]int // openai tool index → anthropic content_block index
	textStarted bool
}

// TransformChunk converts one OpenAI SSE chunk to zero or more Anthropic SSE chunks.
func (t *OpenAIToAnthropic) TransformChunk(chunk []byte) ([]byte, error) {
	_, data, ok := parseSSEChunk(chunk)
	if !ok || len(data) == 0 {
		return nil, nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		// Emit message_stop
		b, _ := json.Marshal(map[string]string{"type": "message_stop"})
		return sseBytes("message_stop", string(b)), nil
	}

	var sc oaStreamChunk
	if err := json.Unmarshal(data, &sc); err != nil {
		return nil, fmt.Errorf("openai stream: parse chunk: %w", err)
	}

	if t.msgID == "" {
		t.msgID = sc.ID
		t.model = sc.Model
		t.toolBlocks = make(map[int]int)
		// Emit message_start
		ms, _ := json.Marshal(map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            t.msgID,
				"type":          "message",
				"role":          "assistant",
				"model":         t.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]int{
					"input_tokens":  0,
					"output_tokens": 0,
				},
			},
		})
		ping, _ := json.Marshal(map[string]string{"type": "ping"})
		return joinSSE(
			sseBytes("message_start", string(ms)),
			sseBytes("ping", string(ping)),
		), nil
	}

	if len(sc.Choices) == 0 {
		return nil, nil
	}

	var out []byte
	ch := sc.Choices[0]
	delta := ch.Delta

	// Text content
	if delta.Content != nil && *delta.Content != "" {
		if !t.textStarted {
			t.textStarted = true
			t.blockIndex++
			cbs, _ := json.Marshal(map[string]any{
				"type":  "content_block_start",
				"index": t.blockIndex - 1,
				"content_block": map[string]string{
					"type": "text",
					"text": "",
				},
			})
			out = append(out, sseBytes("content_block_start", string(cbs))...)
		}
		cbd, _ := json.Marshal(map[string]any{
			"type":  "content_block_delta",
			"index": t.blockIndex - 1,
			"delta": map[string]string{
				"type": "text_delta",
				"text": *delta.Content,
			},
		})
		out = append(out, sseBytes("content_block_delta", string(cbd))...)
	}

	// Tool calls
	for _, tc := range delta.ToolCalls {
		bidx, exists := t.toolBlocks[tc.Index]
		if !exists {
			bidx = t.blockIndex
			t.toolBlocks[tc.Index] = bidx
			t.blockIndex++
			cbs, _ := json.Marshal(map[string]any{
				"type":  "content_block_start",
				"index": bidx,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": map[string]any{},
				},
			})
			out = append(out, sseBytes("content_block_start", string(cbs))...)
		}
		if tc.Function != nil && tc.Function.Arguments != nil && *tc.Function.Arguments != "" {
			cbd, _ := json.Marshal(map[string]any{
				"type":  "content_block_delta",
				"index": bidx,
				"delta": map[string]string{
					"type":         "input_json_delta",
					"partial_json": *tc.Function.Arguments,
				},
			})
			out = append(out, sseBytes("content_block_delta", string(cbd))...)
		}
	}

	// Finish reason
	if ch.FinishReason != nil && *ch.FinishReason != "" {
		// Close any open blocks
		if t.textStarted {
			cbe, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": t.blockIndex - 1 - len(t.toolBlocks)})
			out = append(out, sseBytes("content_block_stop", string(cbe))...)
		}
		for _, bidx := range t.toolBlocks {
			cbe, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": bidx})
			out = append(out, sseBytes("content_block_stop", string(cbe))...)
		}
		md, _ := json.Marshal(map[string]any{
			"type": "message_delta",
			"delta": map[string]string{
				"stop_reason":   mapFinishReason(*ch.FinishReason),
				"stop_sequence": "",
			},
			"usage": map[string]int{"output_tokens": 0},
		})
		out = append(out, sseBytes("message_delta", string(md))...)
	}

	return out, nil
}

// NewStreamTransformer returns a stateful per-chunk transform function for the
// given inbound/upstream shape pair. Returns nil if the pair is not supported.
// Supported pairs: ("anthropic","openai") and ("openai","anthropic").
func NewStreamTransformer(inbound, upstream string) func([]byte) ([]byte, error) {
	switch {
	case inbound == "anthropic" && upstream == "openai":
		// Upstream returns OpenAI chunks; transform to Anthropic SSE.
		t := &OpenAIToAnthropic{}
		return t.TransformChunk
	case inbound == "openai" && upstream == "anthropic":
		// Upstream returns Anthropic chunks; transform to OpenAI SSE.
		t := &AnthropicToOpenAI{}
		return t.TransformChunk
	default:
		return nil
	}
}

// ---- SSE helpers ----

// parseSSEChunk extracts event and data from a raw SSE chunk.
// Handles chunks with or without a leading "event:" line.
func parseSSEChunk(chunk []byte) (event string, data []byte, ok bool) {
	lines := bytes.Split(bytes.TrimRight(chunk, "\n"), []byte("\n"))
	for _, line := range lines {
		if bytes.HasPrefix(line, []byte("event:")) {
			event = string(bytes.TrimSpace(line[6:]))
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data = bytes.TrimSpace(line[5:])
		}
	}
	return event, data, len(data) > 0
}

func sseBytes(event, data string) []byte {
	var b bytes.Buffer
	if event != "" {
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteByte('\n')
	}
	b.WriteString("data: ")
	b.WriteString(data)
	b.WriteString("\n\n")
	return b.Bytes()
}

func joinSSE(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

// ---- minimal local types to avoid import of openai package from tests ----

type oaStreamChunk struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []oaStreamChoice `json:"choices"`
	Usage   *oaUsage         `json:"usage,omitempty"`
}

type oaStreamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role      *string           `json:"role,omitempty"`
	Content   *string           `json:"content,omitempty"`
	ToolCalls []oaToolCallChunk `json:"tool_calls,omitempty"`
}

type oaToolCallChunk struct {
	Index    int            `json:"index"`
	ID       string         `json:"id,omitempty"`
	Type     string         `json:"type,omitempty"`
	Function *oaToolFnChunk `json:"function,omitempty"`
}

type oaToolFnChunk struct {
	Name      string  `json:"name,omitempty"`
	Arguments *string `json:"arguments,omitempty"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
