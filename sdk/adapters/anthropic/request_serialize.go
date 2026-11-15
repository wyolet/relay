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
	// canonical: Metadata dropped — Anthropic metadata accepts only user_id,
	// which User already fills; arbitrary keys 400.
	// canonical: Extensions dropped — no Anthropic-owned extension keys exist;
	// rule 7 has an adapter ignore keys it does not own.
	// canonical: CacheConfig.Key dropped — Anthropic breakpoints are
	// deterministic and need no cache-routing key.

	// max_tokens: always required by Anthropic wire.
	maxTokens := defaultMaxTokensCanonical

	var callerToolChoice *v1.ToolChoice
	var callerParallel *bool

	// Tools are task-level (req.Tools), shared across models — not per-model.
	if tc := req.Tools; tc != nil {
		allowed := allowedToolNames(tc.Choice)
		for _, tool := range tc.Definitions {
			ft, ok := tool.(*v1.FunctionTool)
			if !ok {
				return nil, fmt.Errorf("anthropic serialize_request: unsupported tool type %T", tool)
			}
			if allowed != nil && !allowed[ft.Name] {
				continue
			}
			schema := ft.Parameters
			if schema == nil {
				schema = json.RawMessage(`{}`)
			}
			// canonical: FunctionTool.Strict dropped — Anthropic tool definitions
			// take no strict-schema flag.
			// canonical: FunctionTool.ProviderData dropped — it holds another
			// vendor's original tool definition; Anthropic gets the lowered schema.
			out.Tools = append(out.Tools, anthropicCanonTool{
				Name:                ft.Name,
				Description:         ft.Description,
				InputSchema:         schema,
				EagerInputStreaming: out.Stream,
			})
		}
		callerParallel = tc.Parallel
		if tc.Choice != nil {
			callerToolChoice = tc.Choice
		} else if tc.Parallel != nil && !*tc.Parallel && len(tc.Definitions) > 0 {
			// Anthropic disables parallel calls only via tool_choice, so a
			// parallel=false with no explicit choice rides an "auto" choice.
			callerToolChoice = &v1.ToolChoice{Mode: "auto"}
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
			// canonical: Seed dropped — the Messages API has no seed parameter.
			// canonical: FrequencyPenalty dropped — the Messages API has no
			// frequency penalty.
			// canonical: PresencePenalty dropped — the Messages API has no
			// presence penalty.
		}
		if opts.Reasoning != nil {
			rc := opts.Reasoning
			// Effort passes verbatim: which levels a model accepts is catalog
			// data, so an unsupported value surfaces as the upstream's 400.
			if rc.Effort != "" {
				out.OutputConfig = &anthropicOutputConfig{Effort: rc.Effort}
			}
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
				// canonical: Reasoning.Summary dropped on the budget path — display
				// is set only for adaptive thinking, the mode whose default omits
				// the thinking text.
			} else {
				// No explicit budget → adaptive thinking, the only mode the
				// 4.7+/Sonnet 5/Fable 5 family accepts (budget_tokens 400s there);
				// depth follows output_config.effort. Summary requested → display
				// "summarized" (the family's default "omitted" streams thinking
				// blocks with empty text).
				t := &anthropicCanonThinking{Type: "adaptive"}
				if rc.Summary != "" {
					t.Display = "summarized"
				}
				out.Thinking = t
			}
			// canonical: Temperature/TopP/TopK dropped when reasoning is set —
			// Anthropic rejects custom sampling alongside a thinking block.
			out.Temperature, out.TopP, out.TopK = nil, nil, nil
		}

		// canonical: Output.Verbosity dropped — the Messages API has no
		// verbosity control.
		if opts.Output != nil && opts.Output.Format != nil {
			f := opts.Output.Format
			// canonical: Format.Name/Description/Strict dropped — output_config.format
			// takes only a schema, and its decoding is always constrained.
			switch f.Type {
			case "text":
			case "json_schema", "json_object":
				// Native structured output takes only a strict schema, and an
				// object schema must close additionalProperties, so "any JSON
				// object" has no form here.
				if f.Type == "json_object" || len(f.Schema) == 0 {
					return nil, fmt.Errorf("anthropic serialize_request: %w: structured output needs a JSON schema (json_object has no Anthropic equivalent)", v1.ErrUnsupportedRequest)
				}
				if out.OutputConfig == nil {
					out.OutputConfig = &anthropicOutputConfig{}
				}
				out.OutputConfig.Format = &anthropicOutputFormat{Type: "json_schema", Schema: f.Schema}
			default:
				return nil, fmt.Errorf("anthropic serialize_request: %w: response format type %q", v1.ErrUnsupportedRequest, f.Type)
			}
		}
	}

	if callerToolChoice != nil {
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
	fixedBreakpoints := 0
	if req.CacheConfig != nil && req.CacheConfig.Tools && len(out.Tools) > 0 {
		fixedBreakpoints++
	}
	if systemText != "" {
		if req.CacheConfig != nil && req.CacheConfig.Instructions {
			out.System = withCacheBreakpoint(systemText, cacheTTL)
			fixedBreakpoints++
		} else {
			out.System = systemText
		}
	}

	// cache_config.history → breakpoint on the last input block.
	if req.CacheConfig != nil && req.CacheConfig.History {
		markHistoryBreakpoint(out.Messages, cacheTTL)
	}
	trimMessageBreakpoints(out.Messages, fixedBreakpoints)

	return json.Marshal(out)
}

// allowedToolNames returns the set a choice restricts the model to, or nil when every tool is callable. tool_choice has no allowed list here, so the request carries only these tools; a conversation that changes the list between turns therefore changes `tools`, which invalidates earlier thinking blocks on models that check their prefix.
func allowedToolNames(tc *v1.ToolChoice) map[string]bool {
	if tc == nil || len(tc.Allowed) == 0 || (tc.Mode != "auto" && tc.Mode != "required") {
		return nil
	}
	names := make(map[string]bool, len(tc.Allowed))
	for _, n := range tc.Allowed {
		names[n] = true
	}
	return names
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
