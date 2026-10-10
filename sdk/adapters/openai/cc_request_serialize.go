package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// SerializeRequest encodes a canonical *v1.Request to a CC /v1/chat/completions body.
// SerializeRequest encodes a canonical *v1.Request to a CC /v1/chat/completions body.
func (CCTranslator) SerializeRequest(req *v1.Request) ([]byte, error) {
	if len(req.Model) == 0 {
		return nil, fmt.Errorf("cc serialize_request: model is required")
	}
	model := req.Model[0]

	out := &FullChatRequest{
		Model:    model,
		User:     req.User,
		Metadata: req.Metadata,
	}
	if req.CacheConfig != nil {
		out.PromptCacheKey = req.CacheConfig.Key
		out.PromptCacheRetention = openaiCacheRetention(req.CacheConfig)
		// canonical: CacheConfig.Instructions/Tools dropped — OpenAI caches
		// prefixes automatically; the breakpoint flags have no wire form.
	}
	// canonical: Extensions keys outside the "openai." prefix dropped — rule 7:
	// an adapter ignores keys it does not own.
	if err := ccApplyExtensions(out, req.Extensions); err != nil {
		return nil, fmt.Errorf("cc serialize_request: %w", err)
	}

	// Extract model-specific options.
	if opts, ok := req.ModelConfig[model]; ok && opts != nil {
		if opts.Sampling != nil {
			s := opts.Sampling
			out.Temperature = s.Temperature
			out.TopP = s.TopP
			out.MaxTokens = s.MaxTokens
			out.FrequencyPenalty = s.FrequencyPenalty
			out.PresencePenalty = s.PresencePenalty
			if s.Seed != nil {
				seed := int64(*s.Seed)
				out.Seed = &seed
			}
			if len(s.Stop) > 0 {
				if b, err := json.Marshal(s.Stop); err == nil {
					out.Stop = b
				}
			}
			// canonical: TopK dropped — Chat Completions has no top_k parameter.
		}
		if opts.Reasoning != nil {
			out.ReasoningEffort = opts.Reasoning.Effort
			// canonical: Reasoning.Summary dropped — Chat Completions returns no
			// reasoning summaries and has no parameter to request one.
			// canonical: Reasoning.BudgetTokens dropped — Chat Completions takes
			// an effort level only, no token budget.
		}
		if opts.Output != nil {
			out.Verbosity = opts.Output.Verbosity
			if opts.Output.Format != nil {
				rf, err := ccFormatToResponseFormat(opts.Output.Format)
				if err != nil {
					return nil, err
				}
				out.ResponseFormat = rf
			}
		}
	}

	// Tools are task-level (req.Tools), shared across models — not per-model.
	if tc := req.Tools; tc != nil {
		for _, tool := range tc.Definitions {
			ft, ok := tool.(*v1.FunctionTool)
			if !ok {
				return nil, fmt.Errorf("cc serialize_request: unsupported tool type %T", tool)
			}
			params := ft.Parameters
			if params == nil {
				params = json.RawMessage(`{}`)
			}
			// canonical: FunctionTool.ProviderData dropped — it holds a
			// Responses-only tool definition; CC gets the lowered function schema.
			out.Tools = append(out.Tools, Tool{
				Type: "function",
				Function: FunctionDef{
					Name:        ft.Name,
					Description: ft.Description,
					Parameters:  params,
					Strict:      ft.Strict,
				},
			})
		}
		out.ParallelToolCalls = tc.Parallel
		if tc.Choice != nil {
			b, err := ccToolChoiceFromCanonical(tc.Choice)
			if err != nil {
				return nil, fmt.Errorf("cc serialize_request: %w", err)
			}
			out.ToolChoice = b
		}
	}

	// Stream flag + include_usage so the terminal chunk carries token counts.
	if req.OutputMode == v1.OutputModeStream {
		t := true
		out.Stream = &t
		out.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	// Messages: instructions (+ hoist-flagged system items) → leading system
	// message; other items → messages. Non-hoisted system items stay
	// positional — CC supports the system role anywhere natively.
	input, hoistedSys := v1.SplitHoistedSystem(req.Input)
	instructions := req.Instructions
	if hoistedSys != "" {
		if instructions != "" {
			instructions = instructions + "\n" + hoistedSys
		} else {
			instructions = hoistedSys
		}
	}
	msgs, err := canonicalItemsToCC(instructions, input)
	if err != nil {
		return nil, fmt.Errorf("cc serialize_request: %w", err)
	}
	out.Messages = msgs

	return json.Marshal(out)
}

// hasOpts returns true if any field in opts is set.
func hasOpts(opts *v1.ModelOpts) bool {
	return opts.Sampling != nil || opts.Reasoning != nil || opts.Output != nil
}

// canonicalItemsToCC converts canonical items and instructions to CC messages.
func canonicalItemsToCC(instructions string, items []v1.Item) ([]ChatMessage, error) {
	var msgs []ChatMessage

	if instructions != "" {
		content, _ := json.Marshal(instructions)
		msgs = append(msgs, ChatMessage{Role: "system", Content: content})
	}

	for _, item := range items {
		switch v := item.(type) {
		case *v1.Message:
			msg, err := canonicalMessageToCC(v)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, msg)

		case *v1.FunctionCall:
			// canonical: FunctionCall.ID/Status/ProviderData dropped — a CC tool call carries only its call id, which ParseRequest reuses as the item id.
			// Attach to the last assistant message if possible; otherwise synthesize.
			tc := ToolCall{
				ID:   v.CallID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      v.Name,
					Arguments: v.Arguments,
				},
			}
			if len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
				msgs[len(msgs)-1].ToolCalls = append(msgs[len(msgs)-1].ToolCalls, tc)
			} else {
				nullContent, _ := json.Marshal(nil)
				msgs = append(msgs, ChatMessage{
					Role:      "assistant",
					Content:   nullContent,
					ToolCalls: []ToolCall{tc},
				})
			}

		case *v1.FunctionCallOutput:
			content := ccSerializeFunctionCallOutput(v)
			msgs = append(msgs, ChatMessage{
				Role:       "tool",
				ToolCallID: v.CallID,
				Content:    content,
			})

		case *v1.Reasoning:
			// canonical: reasoning items dropped — CC input has no reasoning
			// message form, and reasoning_content upstreams reject it on input.
		}
	}

	return msgs, nil
}

// ccToolChoice is the CC forced-function form; it nests the name under
// "function", unlike the flat canonical/Responses {type, name} object.
type ccToolChoice struct {
	Type     string `json:"type"`
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

func ccToolChoiceFromCanonical(c *v1.ToolChoice) (json.RawMessage, error) {
	if c.Mode != "function" {
		return json.Marshal(c)
	}
	w := ccToolChoice{Type: "function"}
	w.Function.Name = c.FunctionName
	return json.Marshal(w)
}

// ccToolChoiceToCanonical reads the CC form; the flat form is accepted too
// since lenient clients send it.
func ccToolChoiceToCanonical(raw json.RawMessage) (*v1.ToolChoice, error) {
	choice := &v1.ToolChoice{}
	if err := json.Unmarshal(raw, choice); err != nil {
		return nil, err
	}
	if choice.Mode == "function" && choice.FunctionName == "" {
		var w ccToolChoice
		if err := json.Unmarshal(raw, &w); err == nil {
			choice.FunctionName = w.Function.Name
		}
	}
	return choice, nil
}

// ccFormatToResponseFormat converts a canonical v1.Format to a CC ResponseFormat.
func ccFormatToResponseFormat(f *v1.Format) (*ResponseFormat, error) {
	switch f.Type {
	case "text":
		return nil, nil
	case "json_object":
		return &ResponseFormat{Type: "json_object"}, nil
	case "json_schema":
		inner := map[string]any{
			"name":   f.Name,
			"schema": f.Schema,
		}
		if f.Description != "" {
			inner["description"] = f.Description
		}
		if f.Strict != nil {
			inner["strict"] = *f.Strict
		}
		b, err := json.Marshal(inner)
		if err != nil {
			return nil, fmt.Errorf("cc serialize_request: json_schema format: %w", err)
		}
		return &ResponseFormat{Type: "json_schema", JSONSchema: b}, nil
	default:
		return &ResponseFormat{Type: f.Type}, nil
	}
}

// --- CC → canonical stream ---
