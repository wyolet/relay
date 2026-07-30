package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

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

// canonicalItemsToAnthropic converts canonical []v1.Item to Anthropic messages.
// Returns also the system text merged from LEADING system/developer messages
// (instructions that apply from the start belong in the top-level system
// field). Mid-conversation system/developer items stay positional as
// marker-wrapped user turns, so the cached prefix before them is untouched —
// merging them into the system field would invalidate it. Hoist-flagged items
// must be split off by the caller (v1.SplitHoistedSystem) before this runs.
// cacheTTL is the request-level retention tier applied to item-anchor breakpoints.
func canonicalItemsToAnthropic(items []v1.Item, cacheTTL string) ([]anthropicCanonMsg, string, error) {
	var msgs []anthropicCanonMsg
	var systemParts []string

	// Assistant-run accumulator: consecutive assistant-side items (Reasoning /
	// assistant Message / FunctionCall) coalesce into ONE Anthropic assistant
	// message — Anthropic validates per message that a tool_use-bearing
	// assistant turn leads with its signed thinking block, so splitting a
	// [Reasoning, Message, FunctionCall] run (exactly what ParseResponse yields
	// for a thinking+text+tool_use turn) across two messages 400s on replay.
	// A non-assistant item (user Message, FunctionCallOutput, system) breaks
	// the run. Thinking blocks are hoisted to the front of the flushed message
	// regardless of where the Reasoning items sat in the run — Anthropic
	// requires thinking first when thinking is enabled.
	var runThinking []map[string]any
	var runContents []any // per assistant Message: string or []map[string]any, cache breakpoints pre-applied
	var runToolUses []v1.FunctionCall
	var pendingToolResults []v1.FunctionCallOutput

	// Positional system turns deferred past an open tool round: any message
	// between a tool_use and its tool_result splits the pair (the API rejects
	// anything in that gap), so the turn re-emits right after the tool_result
	// user turn — the slot the API documents for mid-loop instructions.
	var pendingSystem []anthropicCanonMsg
	// seenConversation flips once any non-system item produced or buffered
	// wire content; system/developer items before that merge into the
	// top-level system field, everything after stays positional.
	seenConversation := false

	flushAssistant := func() {
		if len(runThinking) == 0 && len(runContents) == 0 && len(runToolUses) == 0 {
			return
		}
		// A lone assistant Message keeps its original content form
		// (all-text stays a plain string on the wire).
		if len(runThinking) == 0 && len(runToolUses) == 0 && len(runContents) == 1 {
			msgs = append(msgs, anthropicCanonMsg{Role: "assistant", Content: runContents[0]})
			runContents = runContents[:0]
			return
		}
		blocks := make([]map[string]any, 0, len(runThinking)+len(runContents)+len(runToolUses))
		blocks = append(blocks, runThinking...)
		for _, c := range runContents {
			switch c := c.(type) {
			case string:
				if c != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": c})
				}
			case []map[string]any:
				blocks = append(blocks, c...)
			}
		}
		for _, fc := range runToolUses {
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
		runThinking = runThinking[:0]
		runContents = runContents[:0]
		runToolUses = runToolUses[:0]
	}

	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		blocks := make([]map[string]any, 0, len(pendingToolResults))
		for _, fco := range pendingToolResults {
			// Media-carrying results (image parts in fco.Content) emit a block
			// array in part order; text-only results stay a plain string. When
			// Content holds the full part list (anthropic parse keeps text
			// there too), Output duplicates the text — prefer the parts.
			hasMedia := false
			for _, p := range fco.Content {
				if _, ok := p.(*v1.ImagePart); ok {
					hasMedia = true
					break
				}
			}
			var content any
			if hasMedia {
				var contentBlocks []map[string]any
				hasText := false
				for _, p := range fco.Content {
					switch p := p.(type) {
					case *v1.TextPart:
						if p.Text != "" {
							contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": p.Text})
							hasText = true
						}
					case *v1.ImagePart:
						contentBlocks = append(contentBlocks, canonicalImageURLToAnthropicBlock(p.ImageURL))
					}
				}
				if !hasText && fco.Output != "" {
					contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": fco.Output})
				}
				content = contentBlocks
			} else {
				text := fco.Output
				if text == "" && len(fco.Content) > 0 {
					var sb strings.Builder
					for _, p := range fco.Content {
						if tp, ok := p.(*v1.TextPart); ok {
							sb.WriteString(tp.Text)
						}
					}
					text = sb.String()
				}
				content = text
			}
			blocks = append(blocks, map[string]any{
				"type":        "tool_result",
				"tool_use_id": fco.CallID,
				"content":     content,
			})
		}
		msgs = append(msgs, anthropicCanonMsg{Role: "user", Content: blocks})
		pendingToolResults = pendingToolResults[:0]
		msgs = append(msgs, pendingSystem...)
		pendingSystem = pendingSystem[:0]
	}

	for _, item := range items {
		switch v := item.(type) {
		case *v1.Message:
			if v.Role == v1.RoleDeveloper || v.Role == v1.RoleSystem {
				var sb strings.Builder
				for _, p := range v.Content {
					switch tp := p.(type) {
					case *v1.TextPart:
						sb.WriteString(tp.Text)
					case *v1.OutputTextPart:
						sb.WriteString(tp.Text)
					}
				}
				s := sb.String()
				if s == "" {
					continue
				}
				if !seenConversation {
					systemParts = append(systemParts, s)
					continue
				}
				// Positional system items ride as marker-wrapped user turns:
				// works on every model, keeps position, leaves the cached
				// prefix untouched. Real system authority is the caller's
				// explicit choice via hoist (start-of-conversation merge,
				// split off before this loop).
				var content any = v1.WrapSystemMarker(s)
				if v.CacheConfig != nil && v.CacheConfig.Anchor {
					content = withCacheBreakpoint(content, cacheTTL)
				}
				turn := anthropicCanonMsg{Role: "user", Content: content}
				if len(runToolUses) > 0 || len(pendingToolResults) > 0 {
					pendingSystem = append(pendingSystem, turn)
					continue
				}
				flushAssistant()
				msgs = append(msgs, turn)
				continue
			}

			seenConversation = true
			if v.Role != v1.RoleAssistant {
				flushAssistant()
			}
			flushToolResults()

			switch v.Role {
			case v1.RoleUser:
				content, err := canonicalPartsToAnthropicContent(v.Content)
				if err != nil {
					return nil, "", err
				}
				if v.CacheConfig != nil && v.CacheConfig.Anchor {
					content = withCacheBreakpoint(content, cacheTTL)
				}
				msgs = append(msgs, anthropicCanonMsg{Role: "user", Content: content})
			case v1.RoleAssistant:
				content, err := canonicalPartsToAnthropicContent(v.Content)
				if err != nil {
					return nil, "", err
				}
				if v.CacheConfig != nil && v.CacheConfig.Anchor {
					content = withCacheBreakpoint(content, cacheTTL)
				}
				runContents = append(runContents, content)
			}

		case *v1.FunctionCall:
			seenConversation = true
			flushToolResults()
			runToolUses = append(runToolUses, *v)

		case *v1.FunctionCallOutput:
			seenConversation = true
			flushAssistant()
			pendingToolResults = append(pendingToolResults, *v)

		case *v1.Reasoning:
			flushToolResults()
			// Replay signed thinking verbatim: Anthropic requires the thinking
			// block back in the assistant turn that carries its tool_use, and
			// rejects modified or unsigned blocks. Only ProviderData payloads
			// qualify — they hold the exact block (text may legitimately be
			// empty under display "omitted").
			if len(v.ProviderData) > 0 {
				var pd struct {
					Type      string `json:"type"`
					Thinking  string `json:"thinking"`
					Signature string `json:"signature"`
				}
				if err := json.Unmarshal(v.ProviderData, &pd); err == nil && pd.Type == "thinking" && pd.Signature != "" {
					seenConversation = true
					runThinking = append(runThinking, map[string]any{
						"type":      "thinking",
						"thinking":  pd.Thinking,
						"signature": pd.Signature,
					})
					continue
				}
			}
			// canonical: reasoning dropped — no signed Anthropic thinking payload
			// (cross-vendor item, or signature absent); unsigned blocks are
			// rejected upstream, so there is nothing valid to emit.
		}
	}

	if len(runContents) > 0 || len(runToolUses) > 0 {
		flushAssistant()
	}
	// canonical: trailing reasoning dropped — a thinking-only assistant message
	// at the END of the request would be a thinking prefill, which Anthropic
	// rejects when thinking is enabled; mid-history thinking-only runs (broken
	// by a user turn) ARE emitted above.
	flushToolResults()
	msgs = append(msgs, pendingSystem...)

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
