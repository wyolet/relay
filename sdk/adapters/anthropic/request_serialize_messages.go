package anthropic

import (
	"encoding/json"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

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
