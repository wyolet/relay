// Package anthropic — AnthropicTranslator implements v1.Translator for the
// Anthropic Messages wire shape. Converts between Anthropic /v1/messages
// bodies and the canonical v1.Request/v1.Response types.
//
// Design decisions and known lossy mappings:
//   - cache_control on content blocks: dropped on canonical round-trip.
//     Anthropic prompt-caching is a same-vendor optimization; it survives
//     byte-pass but not cross-shape translate.
//   - server_tool_use blocks (web_search, code_execution): dropped. Not
//     modeled in canonical v1 output; per spec comment server tools land in v2.
//   - thinking signature: carried in Reasoning.ProviderData for same-vendor
//     round-trip. Cross-vendor the blob is unusable and dropped on serialize.
//   - pause_turn: maps to StatusIncomplete + IncompleteDetails.Reason="pause_turn".
//   - ping events: silently dropped in stream.
//   - max_tokens: required by Anthropic wire. Defaults to 4096 when canonical
//     SamplingParams.MaxTokens is nil.

package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

const defaultMaxTokensCanonical = 4096

// AnthropicTranslator implements v1.Translator for the Anthropic Messages API.
type AnthropicTranslator struct{}

// ---- wire types (local to this file) ----

type anthropicCanonReq struct {
	Model         string                  `json:"model"`
	System        string                  `json:"system,omitempty"`
	Messages      []anthropicCanonMsg     `json:"messages"`
	Tools         []anthropicCanonTool    `json:"tools,omitempty"`
	ToolChoice    any                     `json:"tool_choice,omitempty"`
	MaxTokens     int                     `json:"max_tokens"`
	Temperature   *float64                `json:"temperature,omitempty"`
	TopP          *float64                `json:"top_p,omitempty"`
	TopK          *int                    `json:"top_k,omitempty"`
	StopSequences []string                `json:"stop_sequences,omitempty"`
	Stream        bool                    `json:"stream,omitempty"`
	Metadata      *anthropicCanonMetadata `json:"metadata,omitempty"`
	Thinking      *anthropicCanonThinking `json:"thinking,omitempty"`
}

type anthropicCanonMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []map[string]any
}

type anthropicCanonTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicCanonMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

type anthropicCanonThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// anthropicFullResp is the full Anthropic response shape used by ParseResponse.
type anthropicFullResp struct {
	ID         string                `json:"id"`
	Type       string                `json:"type"`
	Role       string                `json:"role"`
	Model      string                `json:"model"`
	Content    []anthropicRespBlock  `json:"content"`
	StopReason string                `json:"stop_reason"`
	StopSeq    *string               `json:"stop_sequence,omitempty"`
	Usage      anthropicFullUsage    `json:"usage"`
}

type anthropicRespBlock struct {
	Type string `json:"type"`
	// text block
	Text string `json:"text,omitempty"`
	// tool_use block
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// thinking block
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// citations
	Citations []anthropicCitation `json:"citations,omitempty"`
}

type anthropicFullUsage struct {
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

type anthropicCitation struct {
	Type       string `json:"type"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	StartIndex int    `json:"start_index,omitempty"`
	EndIndex   int    `json:"end_index,omitempty"`
}

// ---- ParseRequest ----

// ParseRequest decodes an Anthropic /v1/messages request body into canonical *v1.Request.
func (AnthropicTranslator) ParseRequest(body []byte) (*v1.Request, error) {
	var wire struct {
		Model         string            `json:"model"`
		System        json.RawMessage   `json:"system"`
		Messages      []json.RawMessage `json:"messages"`
		Tools         []json.RawMessage `json:"tools"`
		ToolChoice    json.RawMessage   `json:"tool_choice"`
		MaxTokens     *int              `json:"max_tokens"`
		Temperature   *float64          `json:"temperature"`
		TopP          *float64          `json:"top_p"`
		TopK          *int              `json:"top_k"`
		StopSequences []string          `json:"stop_sequences"`
		Stream        bool              `json:"stream"`
		Metadata      json.RawMessage   `json:"metadata"`
		Thinking      json.RawMessage   `json:"thinking"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("anthropic parse_request: %w", err)
	}
	if wire.Model == "" {
		return nil, fmt.Errorf("anthropic parse_request: model is required")
	}

	req := &v1.Request{
		Model: v1.ModelRefs{wire.Model},
	}

	if wire.Stream {
		req.OutputMode = v1.OutputModeStream
	} else {
		req.OutputMode = v1.OutputModeSync
	}

	// system → Instructions
	if len(wire.System) > 0 && string(wire.System) != "null" {
		req.Instructions = anthropicExtractSystemText(wire.System)
	}

	// metadata.user_id → User
	if len(wire.Metadata) > 0 && string(wire.Metadata) != "null" {
		var meta struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(wire.Metadata, &meta); err == nil && meta.UserID != "" {
			req.User = meta.UserID
		}
	}

	// Build model opts.
	opts := &v1.ModelOpts{}
	hasOpts := false

	// Sampling
	sp := &v1.SamplingParams{}
	hasSampling := false
	if wire.Temperature != nil {
		sp.Temperature = wire.Temperature
		hasSampling = true
	}
	if wire.TopP != nil {
		sp.TopP = wire.TopP
		hasSampling = true
	}
	if wire.TopK != nil {
		sp.TopK = wire.TopK
		hasSampling = true
	}
	if wire.MaxTokens != nil {
		sp.MaxTokens = wire.MaxTokens
		hasSampling = true
	}
	if len(wire.StopSequences) > 0 {
		sp.Stop = wire.StopSequences
		hasSampling = true
	}
	if hasSampling {
		opts.Sampling = sp
		hasOpts = true
	}

	// Tools
	if len(wire.Tools) > 0 {
		tc := &v1.ToolsConfig{}
		for _, raw := range wire.Tools {
			tool, err := anthropicParseTool(raw)
			if err != nil {
				return nil, fmt.Errorf("anthropic parse_request: tool: %w", err)
			}
			if tool != nil {
				tc.Definitions = append(tc.Definitions, tool)
			}
		}
		if len(wire.ToolChoice) > 0 && string(wire.ToolChoice) != "null" {
			tc.Choice = anthropicParseToolChoice(wire.ToolChoice)
		}
		opts.Tools = tc
		hasOpts = true
	}

	// Thinking → ReasoningConfig
	if len(wire.Thinking) > 0 && string(wire.Thinking) != "null" {
		var thinking struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
			Effort       string `json:"effort"`
		}
		if err := json.Unmarshal(wire.Thinking, &thinking); err == nil {
			if thinking.Type == "enabled" {
				rc := &v1.ReasoningConfig{}
				if thinking.BudgetTokens > 0 {
					rc.BudgetTokens = &thinking.BudgetTokens
				}
				if thinking.Effort != "" {
					rc.Effort = thinking.Effort
				}
				opts.Reasoning = rc
				hasOpts = true
			}
		}
	}

	if hasOpts {
		req.ModelConfig = map[string]*v1.ModelOpts{wire.Model: opts}
	}

	// Build Input from messages.
	input, err := anthropicMessagesToCanonical(wire.Messages)
	if err != nil {
		return nil, fmt.Errorf("anthropic parse_request: messages: %w", err)
	}
	req.Input = input

	return req, nil
}

// ---- SerializeRequest ----

// SerializeRequest encodes a canonical *v1.Request to an Anthropic /v1/messages request body.
func (AnthropicTranslator) SerializeRequest(req *v1.Request) ([]byte, error) {
	if len(req.Model) == 0 {
		return nil, fmt.Errorf("anthropic serialize_request: model is required")
	}
	model := req.Model[0]

	out := &anthropicCanonReq{
		Model:  model,
		System: req.Instructions,
	}

	if req.OutputMode == v1.OutputModeStream {
		out.Stream = true
	}

	// User → metadata.user_id
	if req.User != "" {
		out.Metadata = &anthropicCanonMetadata{UserID: req.User}
	}

	// max_tokens: always required by Anthropic wire.
	maxTokens := defaultMaxTokensCanonical
	if opts, ok := req.ModelConfig[model]; ok && opts != nil {
		if opts.Sampling != nil {
			s := opts.Sampling
			out.Temperature = s.Temperature
			out.TopP = s.TopP
			out.TopK = s.TopK
			if s.MaxTokens != nil {
				maxTokens = *s.MaxTokens
			}
			out.StopSequences = s.Stop
		}
		if opts.Reasoning != nil {
			rc := opts.Reasoning
			thinking := &anthropicCanonThinking{Type: "enabled"}
			if rc.BudgetTokens != nil {
				thinking.BudgetTokens = *rc.BudgetTokens
			}
			if rc.Effort != "" {
				thinking.Type = "enabled"
				// Map effort string to budget_tokens if needed; keep as Effort passthrough.
			}
			out.Thinking = thinking
		}
		if opts.Tools != nil {
			tc := opts.Tools
			for _, tool := range tc.Definitions {
				ft, ok := tool.(*v1.FunctionTool)
				if !ok {
					return nil, fmt.Errorf("anthropic serialize_request: unsupported tool type %T", tool)
				}
				schema := ft.Parameters
				if schema == nil {
					schema = json.RawMessage(`{}`)
				}
				out.Tools = append(out.Tools, anthropicCanonTool{
					Name:        ft.Name,
					Description: ft.Description,
					InputSchema: schema,
				})
			}
			if tc.Choice != nil {
				out.ToolChoice = canonicalToolChoiceToAnthropic(tc.Choice, nil)
			}
		}
	}
	out.MaxTokens = maxTokens

	// Build messages from canonical Input.
	msgs, system, err := canonicalItemsToAnthropic(req.Input)
	if err != nil {
		return nil, fmt.Errorf("anthropic serialize_request: %w", err)
	}
	out.Messages = msgs
	if system != "" {
		if out.System != "" {
			out.System = out.System + "\n" + system
		} else {
			out.System = system
		}
	}

	return json.Marshal(out)
}

// ---- ParseResponse ----

// ParseResponse decodes an Anthropic /v1/messages response body into canonical *v1.Response.
func (AnthropicTranslator) ParseResponse(body []byte) (*v1.Response, error) {
	var ar anthropicFullResp
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("anthropic parse_response: %w", err)
	}

	resp := &v1.Response{
		ID:        ar.ID,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     ar.Model,
	}

	// Map stop_reason.
	resp.Status, resp.FinishReason, resp.IncompleteDetails = anthropicStopReasonToCanonical(ar.StopReason)

	// Build output items from content blocks.
	outputIndex := 0
	for _, block := range ar.Content {
		switch block.Type {
		case "text":
			part := &v1.OutputTextPart{
				Text:        block.Text,
				Annotations: anthropicCitationsToCanonical(block.Citations),
			}
			msg := &v1.Message{
				ID:      fmt.Sprintf("msg_%d", outputIndex),
				Status:  v1.StatusCompleted,
				Role:    v1.RoleAssistant,
				Content: []v1.Part{part},
			}
			resp.Output = append(resp.Output, msg)
			outputIndex++

		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			fc := &v1.FunctionCall{
				ID:        fmt.Sprintf("fc_%d", outputIndex),
				CallID:    block.ID,
				Name:      block.Name,
				Arguments: args,
				Status:    v1.StatusCompleted,
			}
			resp.Output = append(resp.Output, fc)
			outputIndex++

		case "thinking":
			if block.Thinking == "" {
				continue
			}
			// Carry the full thinking block (including signature) in ProviderData for
			// same-vendor round-trip. Cross-vendor consumers ignore ProviderData.
			var providerData json.RawMessage
			if block.Signature != "" {
				pd := map[string]string{
					"type":      "thinking",
					"thinking":  block.Thinking,
					"signature": block.Signature,
				}
				providerData, _ = json.Marshal(pd)
			}
			r := &v1.Reasoning{
				ID:      fmt.Sprintf("rs_%d", outputIndex),
				Content: block.Thinking,
				Summary: []v1.SummaryText{{Text: block.Thinking}},
				Status:  v1.StatusCompleted,
				ProviderData: providerData,
			}
			resp.Output = append(resp.Output, r)
			outputIndex++

		case "redacted_thinking":
			// Cannot faithfully represent; silently drop.

		case "server_tool_use":
			// server_tool_use blocks (web_search, code_execution) not modeled in v1 output.

		default:
			// Unknown block types dropped for forward compatibility.
		}
	}

	// Usage
	total := ar.Usage.InputTokens + ar.Usage.OutputTokens
	resp.Usage = &v1.Usage{
		InputTokens:  ar.Usage.InputTokens,
		OutputTokens: ar.Usage.OutputTokens,
		TotalTokens:  total,
		InputTokensDetails: v1.InputDeets{
			CachedTokens: ar.Usage.CacheReadInputTokens,
		},
	}

	return resp, nil
}

// ---- SerializeResponse ----

// SerializeResponse encodes a canonical *v1.Response to an Anthropic /v1/messages response body.
// req is unused — Anthropic does not require request echo on the response.
func (AnthropicTranslator) SerializeResponse(resp *v1.Response, _ *v1.Request) ([]byte, error) {
	out := map[string]any{
		"id":    resp.ID,
		"type":  "message",
		"role":  "assistant",
		"model": resp.Model,
	}

	// Map canonical status/finish_reason back to Anthropic stop_reason.
	out["stop_reason"] = canonicalFinishReasonToAnthropic(resp.FinishReason, resp.IncompleteDetails)

	// Build content blocks from output items.
	var content []map[string]any
	for _, item := range resp.Output {
		switch v := item.(type) {
		case *v1.Message:
			for _, p := range v.Content {
				switch tp := p.(type) {
				case *v1.OutputTextPart:
					block := map[string]any{
						"type": "text",
						"text": tp.Text,
					}
					if len(tp.Annotations) > 0 {
						block["citations"] = canonicalAnnotationsToAnthropic(tp.Annotations)
					}
					content = append(content, block)
				case *v1.TextPart:
					content = append(content, map[string]any{
						"type": "text",
						"text": tp.Text,
					})
				}
			}
		case *v1.FunctionCall:
			var inputObj any
			if v.Arguments != "" {
				if err := json.Unmarshal([]byte(v.Arguments), &inputObj); err != nil {
					inputObj = map[string]string{"_raw": v.Arguments}
				}
			} else {
				inputObj = map[string]any{}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    v.CallID,
				"name":  v.Name,
				"input": inputObj,
			})
		case *v1.Reasoning:
			// Restore from ProviderData if available; otherwise use Content.
			if len(v.ProviderData) > 0 {
				var pd struct {
					Type      string `json:"type"`
					Thinking  string `json:"thinking"`
					Signature string `json:"signature"`
				}
				if err := json.Unmarshal(v.ProviderData, &pd); err == nil && pd.Type == "thinking" {
					block := map[string]any{
						"type":     "thinking",
						"thinking": pd.Thinking,
					}
					if pd.Signature != "" {
						block["signature"] = pd.Signature
					}
					content = append(content, block)
					continue
				}
			}
			if v.Content != "" {
				content = append(content, map[string]any{
					"type":     "thinking",
					"thinking": v.Content,
				})
			} else if len(v.Summary) > 0 {
				content = append(content, map[string]any{
					"type":     "thinking",
					"thinking": v.Summary[0].Text,
				})
			}
		}
	}
	if content == nil {
		content = []map[string]any{}
	}
	out["content"] = content

	// Usage
	if resp.Usage != nil {
		u := map[string]int{
			"input_tokens":  resp.Usage.InputTokens,
			"output_tokens": resp.Usage.OutputTokens,
		}
		if resp.Usage.InputTokensDetails.CachedTokens > 0 {
			u["cache_read_input_tokens"] = resp.Usage.InputTokensDetails.CachedTokens
		}
		out["usage"] = u
	}

	return json.Marshal(out)
}

// ---- NewToCanonicalStream ----

// NewToCanonicalStream returns a stateful per-stream function that converts
// Anthropic SSE chunks into canonical SSE chunks.
func (AnthropicTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &anthropicToCanonicalStream{}
	return s.translate
}

// ---- NewFromCanonicalStream ----

// NewFromCanonicalStream returns a stateful per-stream function that converts
// canonical SSE chunks into Anthropic SSE chunks.
func (AnthropicTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &canonicalToAnthropicStream{}
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
	// accumulated usage from message_delta
	inputTokens  int
	outputTokens int
	cachedTokens int
	stopReason   string
}

type anthropicStreamBlock struct {
	blockType   string // "text", "tool_use", "thinking"
	outputIndex int
	itemID      string
	textBuf     strings.Builder
	argsBuf     strings.Builder
	thinkBuf    strings.Builder
	callID      string
	toolName    string
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
				InputTokens int `json:"input_tokens"`
				CacheRead   int `json:"cache_read_input_tokens"`
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
	}

	s.currentBlock = b

	var itemType v1.ItemType
	switch blockType {
	case "text":
		itemType = v1.ItemTypeMessage
	case "tool_use":
		itemType = v1.ItemTypeFunctionCall
	case "thinking":
		itemType = v1.ItemTypeReasoning
	default:
		s.currentBlock = nil
		return nil, nil
	}

	startData, _ := json.Marshal(v1.ItemStartedEvent{
		ItemID:   b.itemID,
		ItemType: itemType,
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
		completedItem = &v1.FunctionCall{
			ID:        b.itemID,
			CallID:    b.callID,
			Name:      b.toolName,
			Arguments: b.argsBuf.String(),
			Status:    v1.StatusCompleted,
		}
	case "thinking":
		completedItem = &v1.Reasoning{
			ID:      b.itemID,
			Content: b.thinkBuf.String(),
			Summary: []v1.SummaryText{{Text: b.thinkBuf.String()}},
			Status:  v1.StatusCompleted,
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
	s.stopReason = md.Delta.StopReason
	s.outputTokens = md.Usage.OutputTokens
	return nil, nil
}

func (s *anthropicToCanonicalStream) handleMessageStop() ([]byte, error) {
	status, finish, incomplete := anthropicStopReasonToCanonical(s.stopReason)

	total := s.inputTokens + s.outputTokens
	u := &v1.Usage{
		InputTokens:  s.inputTokens,
		OutputTokens: s.outputTokens,
		TotalTokens:  total,
		InputTokensDetails: v1.InputDeets{
			CachedTokens: s.cachedTokens,
		},
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

// ---- stream: canonical → Anthropic ----

type canonicalToAnthropicStream struct {
	responseID   string
	model        string
	blockIndex   int
	startEmitted bool
}

func (s *canonicalToAnthropicStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	switch event {
	case v1.EventGenerationCreated:
		return s.handleGenerationCreated(data)
	case v1.EventItemStarted:
		return s.handleItemStarted(data)
	case v1.EventItemDelta:
		return s.handleItemDelta(data)
	case v1.EventItemCompleted:
		return s.handleItemCompleted(data)
	case v1.EventGenerationCompleted:
		return s.handleGenerationCompleted(data)
	case v1.EventError:
		return s.handleCanonError(data)
	default:
		return nil, nil
	}
}

func (s *canonicalToAnthropicStream) handleGenerationCreated(data []byte) ([]byte, error) {
	var e v1.GenerationCreatedEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: generation.created: %w", err)
	}
	s.responseID = e.ID
	s.model = e.Model

	// Emit message_start + ping
	ms, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":    s.responseID,
			"type":  "message",
			"role":  "assistant",
			"model": s.model,
			"content": []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]int{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})
	ping, _ := json.Marshal(map[string]string{"type": "ping"})

	var out []byte
	out = append(out, anthropicSSEBytes("message_start", string(ms))...)
	out = append(out, anthropicSSEBytes("ping", string(ping))...)
	return out, nil
}

func (s *canonicalToAnthropicStream) handleItemStarted(data []byte) ([]byte, error) {
	var e v1.ItemStartedEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: item.started: %w", err)
	}

	idx := s.blockIndex
	s.blockIndex++

	var blockType string
	switch e.ItemType {
	case v1.ItemTypeMessage:
		blockType = "text"
	case v1.ItemTypeFunctionCall:
		blockType = "tool_use"
	case v1.ItemTypeReasoning:
		blockType = "thinking"
	default:
		return nil, nil
	}

	cbs, _ := json.Marshal(map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]string{
			"type": blockType,
			"text": "",
		},
	})
	return anthropicSSEBytes("content_block_start", string(cbs)), nil
}

func (s *canonicalToAnthropicStream) handleItemDelta(data []byte) ([]byte, error) {
	var e v1.ItemDeltaEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: item.delta: %w", err)
	}

	// block index is e.Index (matches our started index).
	var deltaType string
	var deltaKey string
	switch e.Kind {
	case v1.DeltaKindText:
		deltaType = "text_delta"
		deltaKey = "text"
	case v1.DeltaKindArguments:
		deltaType = "input_json_delta"
		deltaKey = "partial_json"
	case v1.DeltaKindReasoning:
		deltaType = "thinking_delta"
		deltaKey = "thinking"
	default:
		return nil, nil
	}

	cbd, _ := json.Marshal(map[string]any{
		"type":  "content_block_delta",
		"index": e.Index,
		"delta": map[string]string{
			"type":  deltaType,
			deltaKey: e.Delta,
		},
	})
	return anthropicSSEBytes("content_block_delta", string(cbd)), nil
}

func (s *canonicalToAnthropicStream) handleItemCompleted(data []byte) ([]byte, error) {
	// Only need the index field; Item is polymorphic and not needed here.
	var e struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: item.completed: %w", err)
	}

	cbe, _ := json.Marshal(map[string]any{
		"type":  "content_block_stop",
		"index": e.Index,
	})
	return anthropicSSEBytes("content_block_stop", string(cbe)), nil
}

func (s *canonicalToAnthropicStream) handleGenerationCompleted(data []byte) ([]byte, error) {
	var e v1.GenerationCompletedEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: generation.completed: %w", err)
	}

	stopReason := canonicalFinishReasonToAnthropicStr(e.FinishReason)
	if e.Status == v1.StatusIncomplete && e.FinishReason == "" {
		stopReason = "pause_turn"
	}

	outTokens := 0
	if e.Usage != nil {
		outTokens = e.Usage.OutputTokens
	}

	md, _ := json.Marshal(map[string]any{
		"type": "message_delta",
		"delta": map[string]string{
			"stop_reason":   stopReason,
			"stop_sequence": "",
		},
		"usage": map[string]int{"output_tokens": outTokens},
	})
	ms, _ := json.Marshal(map[string]string{"type": "message_stop"})

	var out []byte
	out = append(out, anthropicSSEBytes("message_delta", string(md))...)
	out = append(out, anthropicSSEBytes("message_stop", string(ms))...)
	return out, nil
}

func (s *canonicalToAnthropicStream) handleCanonError(data []byte) ([]byte, error) {
	var e v1.ErrorEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("canonical→anthropic: error: %w", err)
	}
	errB, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    e.Code,
			"message": e.Message,
		},
	})
	return anthropicSSEBytes("error", string(errB)), nil
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

// anthropicExtractSystemText handles system being a plain string or an array
// of {type:"text", text:"..."} blocks.
func anthropicExtractSystemText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicParseTool decodes one raw tool JSON into a canonical v1.Tool.
// Anthropic server tools (web_search_20250305 etc.) are mapped to ServerTool.
func anthropicParseTool(raw json.RawMessage) (v1.Tool, error) {
	var probe struct {
		Name        string          `json:"name"`
		Type        string          `json:"type"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, err
	}
	// Anthropic server tools have type != "" (e.g. "web_search_20250305").
	if probe.Type != "" && probe.Type != "function" {
		return &v1.ServerTool{Name: probe.Name}, nil
	}
	schema := probe.InputSchema
	if schema == nil {
		schema = json.RawMessage(`{}`)
	}
	return &v1.FunctionTool{
		Name:        probe.Name,
		Description: probe.Description,
		Parameters:  schema,
	}, nil
}

// anthropicParseToolChoice decodes Anthropic tool_choice JSON into canonical *v1.ToolChoice.
func anthropicParseToolChoice(raw json.RawMessage) *v1.ToolChoice {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return &v1.ToolChoice{Mode: "auto"}
	case "any":
		return &v1.ToolChoice{Mode: "required"}
	case "none":
		return &v1.ToolChoice{Mode: "none"}
	case "tool":
		return &v1.ToolChoice{Mode: "function", FunctionName: tc.Name}
	default:
		return &v1.ToolChoice{Mode: tc.Type}
	}
}

// canonicalToolChoiceToAnthropic converts canonical ToolChoice → Anthropic tool_choice map.
// parallelDisable adds disable_parallel_tool_use when not nil and false.
func canonicalToolChoiceToAnthropic(tc *v1.ToolChoice, parallel *bool) map[string]any {
	disableParallel := parallel != nil && !*parallel
	switch tc.Mode {
	case "auto":
		m := map[string]any{"type": "auto"}
		if disableParallel {
			m["disable_parallel_tool_use"] = true
		}
		return m
	case "required":
		m := map[string]any{"type": "any"}
		if disableParallel {
			m["disable_parallel_tool_use"] = true
		}
		return m
	case "none":
		return map[string]any{"type": "none"}
	case "function":
		m := map[string]any{"type": "tool", "name": tc.FunctionName}
		if disableParallel {
			m["disable_parallel_tool_use"] = true
		}
		return m
	default:
		return map[string]any{"type": "auto"}
	}
}

// anthropicMessagesToCanonical converts Anthropic messages to canonical []v1.Item.
// Each message role maps directly. Content blocks within each message are parsed.
func anthropicMessagesToCanonical(raws []json.RawMessage) ([]v1.Item, error) {
	var items []v1.Item
	for _, raw := range raws {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, err
		}

		switch msg.Role {
		case "user":
			parts, err := anthropicContentToCanonicalParts(msg.Content)
			if err != nil {
				return nil, err
			}
			// tool_result blocks inside user messages become FunctionCallOutput items.
			toolResults, textParts := splitToolResults(parts, msg.Content)
			for _, tr := range toolResults {
				items = append(items, tr)
			}
			if len(textParts) > 0 {
				items = append(items, &v1.Message{Role: v1.RoleUser, Content: textParts})
			}

		case "assistant":
			msgItem, toolCalls, err := anthropicAssistantContentToItems(msg.Content)
			if err != nil {
				return nil, err
			}
			if msgItem != nil {
				items = append(items, msgItem)
			}
			items = append(items, toolCalls...)

		default:
			// Unknown roles become user messages.
			parts, _ := anthropicContentToCanonicalParts(msg.Content)
			if len(parts) > 0 {
				items = append(items, &v1.Message{Role: v1.RoleUser, Content: parts})
			}
		}
	}
	return items, nil
}

// anthropicContentToCanonicalParts converts Anthropic content (string or []block) to canonical []Part.
func anthropicContentToCanonicalParts(raw json.RawMessage) ([]v1.Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// Plain string
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []v1.Part{&v1.TextPart{Text: s}}, nil
	}
	// Array of blocks
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	var parts []v1.Part
	for _, b := range blocks {
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "text":
			var block struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b, &block)
			parts = append(parts, &v1.TextPart{Text: block.Text})
		case "image":
			url := anthropicImageBlockToURL(b)
			if url != "" {
				parts = append(parts, &v1.ImagePart{ImageURL: url})
			}
		case "tool_result":
			// handled separately in splitToolResults
		}
	}
	return parts, nil
}

// splitToolResults extracts tool_result blocks from raw content and returns them as
// FunctionCallOutput items + remaining text/image parts.
func splitToolResults(parts []v1.Part, raw json.RawMessage) ([]*v1.FunctionCallOutput, []v1.Part) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, parts
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, parts
	}
	var toolResults []*v1.FunctionCallOutput
	var textParts []v1.Part
	for _, b := range blocks {
		var probe struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			continue
		}
		if probe.Type == "tool_result" {
			output := ""
			if len(probe.Content) > 0 {
				// content can be string or array of text blocks
				if probe.Content[0] == '"' {
					_ = json.Unmarshal(probe.Content, &output)
				} else {
					var contentParts []v1.Part
					contentParts, _ = anthropicContentToCanonicalParts(probe.Content)
					for _, p := range contentParts {
						if tp, ok := p.(*v1.TextPart); ok {
							output += tp.Text
						}
					}
				}
			}
			toolResults = append(toolResults, &v1.FunctionCallOutput{
				CallID: probe.ToolUseID,
				Output: output,
			})
		} else {
			// Re-extract as part
			switch probe.Type {
			case "text":
				var block struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(b, &block)
				textParts = append(textParts, &v1.TextPart{Text: block.Text})
			case "image":
				url := anthropicImageBlockToURL(b)
				if url != "" {
					textParts = append(textParts, &v1.ImagePart{ImageURL: url})
				}
			}
		}
	}
	return toolResults, textParts
}

// anthropicAssistantContentToItems converts an assistant message content to canonical items.
// Returns a Message item (for text content) and FunctionCall items (for tool_use blocks).
func anthropicAssistantContentToItems(raw json.RawMessage) (*v1.Message, []v1.Item, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return &v1.Message{Role: v1.RoleAssistant}, nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, nil, err
		}
		return &v1.Message{
			Role:    v1.RoleAssistant,
			Content: []v1.Part{&v1.OutputTextPart{Text: s}},
		}, nil, nil
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, nil, err
	}

	var textParts []v1.Part
	var toolItems []v1.Item
	for _, b := range blocks {
		var probe struct {
			Type      string          `json:"type"`
			Text      string          `json:"text,omitempty"`
			ID        string          `json:"id,omitempty"`
			Name      string          `json:"name,omitempty"`
			Input     json.RawMessage `json:"input,omitempty"`
			Thinking  string          `json:"thinking,omitempty"`
			Signature string          `json:"signature,omitempty"`
		}
		if err := json.Unmarshal(b, &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "text":
			textParts = append(textParts, &v1.OutputTextPart{Text: probe.Text})
		case "tool_use":
			args := "{}"
			if len(probe.Input) > 0 {
				args = string(probe.Input)
			}
			toolItems = append(toolItems, &v1.FunctionCall{
				ID:        probe.ID,
				CallID:    probe.ID,
				Name:      probe.Name,
				Arguments: args,
			})
		case "thinking":
			var pd json.RawMessage
			if probe.Signature != "" {
				pdMap := map[string]string{
					"type":      "thinking",
					"thinking":  probe.Thinking,
					"signature": probe.Signature,
				}
				pd, _ = json.Marshal(pdMap)
			}
			toolItems = append(toolItems, &v1.Reasoning{
				Content:      probe.Thinking,
				Summary:      []v1.SummaryText{{Text: probe.Thinking}},
				ProviderData: pd,
			})
		}
	}

	var msgItem *v1.Message
	if len(textParts) > 0 || len(toolItems) == 0 {
		msgItem = &v1.Message{Role: v1.RoleAssistant, Content: textParts}
	}
	return msgItem, toolItems, nil
}

// anthropicImageBlockToURL converts an Anthropic image content block to a URL string.
func anthropicImageBlockToURL(raw json.RawMessage) string {
	var block struct {
		Source struct {
			Type      string `json:"type"`
			URL       string `json:"url,omitempty"`
			MediaType string `json:"media_type,omitempty"`
			Data      string `json:"data,omitempty"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return ""
	}
	switch block.Source.Type {
	case "url":
		return block.Source.URL
	case "base64":
		mt := block.Source.MediaType
		if mt == "" {
			mt = "application/octet-stream"
		}
		return "data:" + mt + ";base64," + block.Source.Data
	}
	return ""
}

// canonicalItemsToAnthropic converts canonical []v1.Item to Anthropic messages.
// Returns also any additional system text from developer-role messages.
func canonicalItemsToAnthropic(items []v1.Item) ([]anthropicCanonMsg, string, error) {
	var msgs []anthropicCanonMsg
	var systemParts []string

	var pendingToolUses []v1.FunctionCall
	var pendingToolResults []v1.FunctionCallOutput

	flushToolUses := func() {
		if len(pendingToolUses) == 0 {
			return
		}
		blocks := make([]map[string]any, 0, len(pendingToolUses))
		for _, fc := range pendingToolUses {
			var inputObj any
			if fc.Arguments != "" {
				if err := json.Unmarshal([]byte(fc.Arguments), &inputObj); err != nil {
					inputObj = map[string]string{"_raw": fc.Arguments}
				}
			} else {
				inputObj = map[string]any{}
			}
			blocks = append(blocks, map[string]any{
				"type":  "tool_use",
				"id":    fc.CallID,
				"name":  fc.Name,
				"input": inputObj,
			})
		}
		msgs = append(msgs, anthropicCanonMsg{Role: "assistant", Content: blocks})
		pendingToolUses = pendingToolUses[:0]
	}

	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		blocks := make([]map[string]any, 0, len(pendingToolResults))
		for _, fco := range pendingToolResults {
			content := fco.Output
			if content == "" && len(fco.Content) > 0 {
				var sb strings.Builder
				for _, p := range fco.Content {
					if tp, ok := p.(*v1.TextPart); ok {
						sb.WriteString(tp.Text)
					}
				}
				content = sb.String()
			}
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": fco.CallID,
				"content":     content,
			})
		}
		msgs = append(msgs, anthropicCanonMsg{Role: "user", Content: blocks})
		pendingToolResults = pendingToolResults[:0]
	}

	for _, item := range items {
		switch v := item.(type) {
		case *v1.Message:
			flushToolUses()
			flushToolResults()

			switch v.Role {
			case v1.RoleDeveloper, v1.RoleSystem:
				// Collect as additional system text.
				var sb strings.Builder
				for _, p := range v.Content {
					switch tp := p.(type) {
					case *v1.TextPart:
						sb.WriteString(tp.Text)
					case *v1.OutputTextPart:
						sb.WriteString(tp.Text)
					}
				}
				if s := sb.String(); s != "" {
					systemParts = append(systemParts, s)
				}
				continue
			case v1.RoleUser:
				content, err := canonicalPartsToAnthropicContent(v.Content)
				if err != nil {
					return nil, "", err
				}
				msgs = append(msgs, anthropicCanonMsg{Role: "user", Content: content})
			case v1.RoleAssistant:
				content, err := canonicalPartsToAnthropicContent(v.Content)
				if err != nil {
					return nil, "", err
				}
				msgs = append(msgs, anthropicCanonMsg{Role: "assistant", Content: content})
			}

		case *v1.FunctionCall:
			flushToolResults()
			pendingToolUses = append(pendingToolUses, *v)

		case *v1.FunctionCallOutput:
			flushToolUses()
			pendingToolResults = append(pendingToolResults, *v)

		case *v1.Reasoning:
			// Drop reasoning items when serializing to Anthropic upstream.
			// Anthropic manages its own thinking; we don't echo it back.
		}
	}

	flushToolUses()
	flushToolResults()

	return msgs, strings.Join(systemParts, "\n"), nil
}

// canonicalPartsToAnthropicContent converts canonical []v1.Part to Anthropic content.
// All-text → plain string. Mixed → array of blocks.
func canonicalPartsToAnthropicContent(parts []v1.Part) (any, error) {
	if len(parts) == 0 {
		return "", nil
	}
	allText := true
	for _, p := range parts {
		switch p.PartType() {
		case v1.PartTypeInputText, v1.PartTypeOutputText:
		default:
			allText = false
		}
	}
	if allText {
		var sb strings.Builder
		for _, p := range parts {
			switch v := p.(type) {
			case *v1.TextPart:
				sb.WriteString(v.Text)
			case *v1.OutputTextPart:
				sb.WriteString(v.Text)
			}
		}
		return sb.String(), nil
	}

	blocks := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		block, err := canonicalPartToAnthropicBlock(p)
		if err != nil {
			return nil, err
		}
		if block != nil {
			blocks = append(blocks, block)
		}
	}
	return blocks, nil
}

// canonicalPartToAnthropicBlock converts one canonical Part to an Anthropic content block.
func canonicalPartToAnthropicBlock(p v1.Part) (map[string]any, error) {
	switch v := p.(type) {
	case *v1.TextPart:
		return map[string]any{"type": "text", "text": v.Text}, nil
	case *v1.OutputTextPart:
		return map[string]any{"type": "text", "text": v.Text}, nil
	case *v1.ImagePart:
		return canonicalImageURLToAnthropicBlock(v.ImageURL), nil
	case *v1.FilePart:
		if v.FileData != "" {
			mt := "application/pdf"
			if v.MediaType != "" {
				mt = v.MediaType
			}
			return map[string]any{
				"type": "document",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mt,
					"data":       v.FileData,
				},
			}, nil
		}
		if v.FileURL != "" {
			return map[string]any{
				"type": "document",
				"source": map[string]any{
					"type": "url",
					"url":  v.FileURL,
				},
			}, nil
		}
		return nil, fmt.Errorf("anthropic serialize_request: file part has no data or URL")
	default:
		return nil, fmt.Errorf("anthropic serialize_request: unsupported part type %T", p)
	}
}

// canonicalImageURLToAnthropicBlock converts a canonical image URL to an Anthropic image block.
func canonicalImageURLToAnthropicBlock(url string) map[string]any {
	if strings.HasPrefix(url, "data:") {
		rest := url[5:]
		semi := strings.Index(rest, ";")
		comma := strings.Index(rest, ",")
		if semi >= 0 && comma > semi {
			mt := rest[:semi]
			data := rest[comma+1:]
			return map[string]any{
				"type": "image",
				"source": map[string]any{
					"type":       "base64",
					"media_type": mt,
					"data":       data,
				},
			}
		}
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type": "url",
			"url":  url,
		},
	}
}

// anthropicStopReasonToCanonical maps an Anthropic stop_reason to canonical status/finish/incomplete.
func anthropicStopReasonToCanonical(reason string) (v1.Status, v1.FinishReason, *v1.IncompleteDetails) {
	switch reason {
	case "end_turn", "stop_sequence", "":
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	case "max_tokens":
		return v1.StatusIncomplete, v1.FinishReasonLength, &v1.IncompleteDetails{Reason: "max_output_tokens"}
	case "tool_use":
		return v1.StatusCompleted, v1.FinishReasonToolCalls, nil
	case "refusal":
		return v1.StatusCompleted, v1.FinishReasonRefusal, nil
	case "pause_turn":
		return v1.StatusIncomplete, "", &v1.IncompleteDetails{Reason: "pause_turn"}
	default:
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	}
}

// canonicalFinishReasonToAnthropic maps canonical finish_reason + incomplete_details to Anthropic stop_reason string.
func canonicalFinishReasonToAnthropic(reason v1.FinishReason, incomplete *v1.IncompleteDetails) string {
	if incomplete != nil {
		switch incomplete.Reason {
		case "max_output_tokens":
			return "max_tokens"
		case "pause_turn":
			return "pause_turn"
		}
	}
	return canonicalFinishReasonToAnthropicStr(reason)
}

func canonicalFinishReasonToAnthropicStr(reason v1.FinishReason) string {
	switch reason {
	case v1.FinishReasonStop:
		return "end_turn"
	case v1.FinishReasonLength:
		return "max_tokens"
	case v1.FinishReasonToolCalls:
		return "tool_use"
	case v1.FinishReasonRefusal:
		return "refusal"
	case v1.FinishReasonContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

// anthropicCitationsToCanonical maps Anthropic url_citation annotations to canonical Annotations.
func anthropicCitationsToCanonical(cits []anthropicCitation) []v1.Annotation {
	if len(cits) == 0 {
		return nil
	}
	var out []v1.Annotation
	for _, c := range cits {
		if c.Type == "url_citation" {
			out = append(out, &v1.URLCitationAnnotation{
				URL:        c.URL,
				Title:      c.Title,
				StartIndex: c.StartIndex,
				EndIndex:   c.EndIndex,
			})
		}
		// char_location and page_location dropped — no clean v1 equivalent.
	}
	return out
}

// canonicalAnnotationsToAnthropic maps canonical annotations to Anthropic citations.
func canonicalAnnotationsToAnthropic(anns []v1.Annotation) []map[string]any {
	var out []map[string]any
	for _, a := range anns {
		switch v := a.(type) {
		case *v1.URLCitationAnnotation:
			out = append(out, map[string]any{
				"type":        "url_citation",
				"url":         v.URL,
				"title":       v.Title,
				"start_index": v.StartIndex,
				"end_index":   v.EndIndex,
			})
		}
	}
	return out
}
