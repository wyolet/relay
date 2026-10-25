package anthropic

import (
	"github.com/wyolet/relay/sdk/internal/jsonscan"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// messageContent is a message's content read once: a string, blocks, or
// neither (absent, null, or another JSON type).
type messageContent struct {
	text     string
	isText   bool
	blocks   []contentBlock
	isBlocks bool
	mistyped bool // neither string, array nor null
}

func readMessageContent(raw []byte) (messageContent, bool) {
	var c messageContent
	switch {
	case raw == nil || jsonscan.IsNull(raw):
	case raw[0] == '"':
		c.text, c.isText = unquote(raw), true
	case raw[0] == '[':
		var ok bool
		if c.blocks, ok = readContentBlocks(raw); !ok {
			return c, false
		}
		c.isBlocks = true
	default:
		c.mistyped = true
	}
	return c, true
}

// parts mirrors anthropicContentToCanonicalParts.
func (c *messageContent) parts() []v1.Part {
	if c.isText {
		return []v1.Part{&v1.TextPart{Text: c.text}}
	}
	var parts []v1.Part
	for i := range c.blocks {
		if p := blockPart(&c.blocks[i]); p != nil {
			parts = append(parts, p)
		}
	}
	return parts
}

// anchor mirrors contentAnchor.
func (c *messageContent) anchor(breakpoints *cacheBreakpoints) *v1.ItemCacheConfig {
	var anchor *v1.ItemCacheConfig
	for i := range c.blocks {
		switch b := &c.blocks[i]; b.typ {
		case "text", "image", "document":
			if a := breakpoints.anchorIfMarked(b.cacheControl); a != nil {
				anchor = a
			}
		}
	}
	return anchor
}

// blockPart mirrors anthropicBlockToPart.
func blockPart(b *contentBlock) v1.Part {
	var src blockSource
	if b.source != nil {
		src = *b.source
	}
	switch b.typ {
	case "text":
		return &v1.TextPart{Text: b.text}
	case "image":
		if url := imageSourceToURL(src); url != "" {
			return &v1.ImagePart{ImageURL: url}
		}
	case "document":
		if p := documentSourceToPart(src); p != nil {
			return p
		}
	}
	return nil
}

// appendMessageItems mirrors one iteration of anthropicMessagesToCanonical;
// ok is false where that rejects the message.
func appendMessageItems(items []v1.Item, role string, raw []byte, breakpoints *cacheBreakpoints) ([]v1.Item, bool) {
	c, ok := readMessageContent(raw)
	if !ok {
		return nil, false
	}
	switch role {
	case "user":
		if c.mistyped {
			return nil, false
		}
		toolResults, textParts, ok := c.splitToolResults(breakpoints)
		if !ok {
			return nil, false
		}
		for _, tr := range toolResults {
			items = append(items, tr)
		}
		anchor := c.anchor(breakpoints)
		if sysItem := unwrapSystemUserTurn(toolResults, textParts); sysItem != nil {
			sysItem.CacheConfig = anchor
			items = append(items, sysItem)
		} else if len(textParts) > 0 {
			items = append(items, &v1.Message{Role: v1.RoleUser, Content: textParts, CacheConfig: anchor})
		}

	case "system":
		if c.mistyped {
			return nil, false
		}
		if parts := c.parts(); len(parts) > 0 {
			items = append(items, &v1.Message{Role: v1.RoleSystem, Content: parts, CacheConfig: c.anchor(breakpoints)})
		}

	case "assistant":
		if c.mistyped {
			return nil, false
		}
		msgItem, toolCalls := c.assistantItems(breakpoints)
		if msgItem != nil {
			msgItem.CacheConfig = c.anchor(breakpoints)
			items = append(items, msgItem)
		}
		items = append(items, toolCalls...)

	default:
		if parts := c.parts(); len(parts) > 0 {
			items = append(items, &v1.Message{Role: v1.RoleUser, Content: parts, CacheConfig: c.anchor(breakpoints)})
		}
	}
	return items, true
}

// splitToolResults mirrors splitToolResults.
func (c *messageContent) splitToolResults(breakpoints *cacheBreakpoints) ([]*v1.FunctionCallOutput, []v1.Part, bool) {
	if !c.isBlocks {
		return nil, c.parts(), true
	}
	var toolResults []*v1.FunctionCallOutput
	var textParts []v1.Part
	for i := range c.blocks {
		b := &c.blocks[i]
		if b.typ != "tool_result" {
			if p := blockPart(b); p != nil {
				textParts = append(textParts, p)
			}
			continue
		}
		output, media, ok := toolResultOutput(b.content)
		if !ok {
			return nil, nil, false
		}
		toolResults = append(toolResults, &v1.FunctionCallOutput{
			CallID:      b.toolUseID,
			Output:      output,
			Content:     media,
			CacheConfig: breakpoints.anchorIfMarked(b.cacheControl),
		})
	}
	return toolResults, textParts, true
}

// toolResultOutput mirrors the tool_result branch of splitToolResults: text
// joins into the output string, and media keeps the whole part list.
func toolResultOutput(raw []byte) (string, []v1.Part, bool) {
	c, ok := readMessageContent(raw)
	if !ok {
		return "", nil, false
	}
	if c.isText {
		return c.text, nil, true
	}
	output := ""
	hasMedia := false
	parts := c.parts()
	for _, p := range parts {
		if tp, ok := p.(*v1.TextPart); ok {
			output += tp.Text
		} else {
			hasMedia = true
		}
	}
	if hasMedia {
		return output, parts, true
	}
	return output, nil, true
}

// assistantItems mirrors anthropicAssistantContentToItems.
func (c *messageContent) assistantItems(breakpoints *cacheBreakpoints) (*v1.Message, []v1.Item) {
	if c.isText {
		return &v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.OutputTextPart{Text: c.text}}}, nil
	}
	var textParts []v1.Part
	var toolItems []v1.Item
	for i := range c.blocks {
		switch b := &c.blocks[i]; b.typ {
		case "text":
			textParts = append(textParts, &v1.OutputTextPart{Text: b.text})
		case "tool_use":
			args := "{}"
			if b.input != nil {
				args = string(b.input)
			}
			toolItems = append(toolItems, &v1.FunctionCall{
				ID:          b.id,
				CallID:      b.id,
				Name:        b.name,
				Arguments:   args,
				CacheConfig: breakpoints.anchorIfMarked(b.cacheControl),
			})
		case "thinking":
			toolItems = append(toolItems, thinkingToReasoning(b.thinking, b.signature))
		}
	}
	if len(textParts) > 0 || len(toolItems) == 0 {
		return &v1.Message{Role: v1.RoleAssistant, Content: textParts}, toolItems
	}
	return nil, toolItems
}
