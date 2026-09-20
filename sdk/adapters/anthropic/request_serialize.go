package anthropic

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- SerializeRequest ----

// SerializeRequest encodes a canonical *v1.Request to an Anthropic /v1/messages request body.
func (AnthropicTranslator) SerializeRequest(req *v1.Request) ([]byte, error) {
	if len(req.Model) == 0 {
		return nil, fmt.Errorf("anthropic serialize_request: model is required")
	}
	model := req.Model[0]

	out := &anthropicCanonReq{
		Model: model,
	}
	systemText := req.Instructions

	if req.OutputMode == v1.OutputModeStream {
		out.Stream = true
	}

	// User → metadata.user_id
	if req.User != "" {
		out.Metadata = &anthropicCanonMetadata{UserID: req.User}
	}

	// max_tokens: always required by Anthropic wire.
	maxTokens := defaultMaxTokensCanonical

	// Track the caller's tool choice separately so we can decide whether to
	// override it with the structured-output forced-tool choice below.
	var callerToolChoice *v1.ToolChoice
	var callerParallel *bool

	// Tools are task-level (req.Tools), shared across models — not per-model.
	if tc := req.Tools; tc != nil {
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
				Name:                ft.Name,
				Description:         ft.Description,
				InputSchema:         schema,
				EagerInputStreaming: out.Stream,
			})
		}
		if tc.Choice != nil {
			callerToolChoice = tc.Choice
			callerParallel = tc.Parallel
		}
	}

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
			if rc.BudgetTokens != nil && *rc.BudgetTokens > 0 {
				// Explicit budget → legacy manual extended thinking. This is the
				// escape hatch for pre-4.6 models, which reject type "adaptive";
				// clamp to Anthropic's 1024 floor and ensure max_tokens leaves room
				// past the budget (Anthropic requires max_tokens > budget_tokens).
				budget := *rc.BudgetTokens
				if budget < 1024 {
					budget = 1024
				}
				if maxTokens <= budget {
					maxTokens = budget + 4096 // headroom for the visible answer beyond the thinking
				}
				out.Thinking = &anthropicCanonThinking{Type: "enabled", BudgetTokens: budget}
			} else {
				// No explicit budget → adaptive thinking, the only mode the
				// 4.7+/Sonnet 5/Fable 5 family accepts (budget_tokens 400s there).
				// Anthropic has no effort knob on the wire, so canonical Effort maps
				// to adaptive and the model self-calibrates depth. Summary requested
				// → display "summarized" (the family's default "omitted" streams
				// thinking blocks with empty text).
				t := &anthropicCanonThinking{Type: "adaptive"}
				if rc.Summary != "" {
					t.Display = "summarized"
				}
				out.Thinking = t
			}
			// Thinking is incompatible with custom sampling — Anthropic rejects
			// temperature/top_p/top_k alongside an enabled/adaptive thinking block.
			out.Temperature, out.TopP, out.TopK = nil, nil, nil
		}

		// Structured output via forced-tool trick. Anthropic has no native
		// response_format/json_schema param, so we inject a synthetic tool and
		// force the model to call it. ParseResponse/stream unwrap it back to
		// plain text so the caller sees a normal completed text response.
		if opts.Output != nil && opts.Output.Format != nil {
			f := opts.Output.Format
			if f.Type == "json_schema" || f.Type == "json_object" {
				// canonical: Output.Format ignored when caller forces their own
				// tool choice — their explicit intent wins over structured output.
				callerForces := callerToolChoice != nil &&
					(callerToolChoice.Mode == "required" || callerToolChoice.Mode == "function")
				if !callerForces {
					schema := f.Schema
					if len(schema) == 0 || f.Type == "json_object" {
						schema = defaultJSONObjectSchema
					}
					out.Tools = append(out.Tools, anthropicCanonTool{
						Name:                structuredOutputToolName,
						Description:         "Return the response as JSON.",
						InputSchema:         schema,
						EagerInputStreaming: out.Stream,
					})
					out.ToolChoice = map[string]any{
						"type": "tool",
						"name": structuredOutputToolName,
					}
				}
			}
		}
	}

	// Apply the caller's tool choice only when structured-output didn't override it.
	if out.ToolChoice == nil && callerToolChoice != nil {
		out.ToolChoice = canonicalToolChoiceToAnthropic(callerToolChoice, callerParallel)
	}

	out.MaxTokens = maxTokens

	// cache_config.ttl → retention tier applied to every breakpoint this
	// request emits (tools / instructions / item anchors).
	cacheTTL := anthropicCacheTTL(req.CacheConfig)

	// cache_config.tools → breakpoint on the last tool (caches the tools block).
	if req.CacheConfig != nil && req.CacheConfig.Tools && len(out.Tools) > 0 {
		out.Tools[len(out.Tools)-1].CacheControl = anthropicEphemeralCacheControl(cacheTTL)
	}

	// Build messages from canonical Input. Per-message cache anchors are applied
	// inside (each Message carries its own ItemCacheConfig). The system prefix
	// merges, in order: Instructions, leading system/developer items, then
	// hoist-flagged items; non-hoisted positional system items ride in msgs.
	input, hoistedSys := v1.SplitHoistedSystem(req.Input)
	msgs, sysFromItems, err := canonicalItemsToAnthropic(input, cacheTTL)
	if err != nil {
		return nil, fmt.Errorf("anthropic serialize_request: %w", err)
	}
	out.Messages = msgs
	for _, extra := range []string{sysFromItems, hoistedSys} {
		if extra == "" {
			continue
		}
		if systemText != "" {
			systemText = systemText + "\n" + extra
		} else {
			systemText = extra
		}
	}

	// cache_config.instructions → breakpoint on the system prefix. Coerces the
	// system string to a single text block so cache_control can ride on it.
	if systemText != "" {
		if req.CacheConfig != nil && req.CacheConfig.Instructions {
			out.System = withCacheBreakpoint(systemText, cacheTTL)
		} else {
			out.System = systemText
		}
	}

	return json.Marshal(out)
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
