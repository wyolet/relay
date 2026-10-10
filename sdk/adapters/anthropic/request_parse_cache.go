package anthropic

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// cacheBreakpoints reads wire cache_control markers back into canonical cache intent, the inverse of the breakpoints SerializeRequest emits. It collects the TTL tier across the whole request, since canonical TTL is request-wide.
type cacheBreakpoints struct {
	longTTL bool
}

type cacheControlProbe struct {
	Type         string `json:"type"`
	CacheControl *struct {
		TTL string `json:"ttl"`
	} `json:"cache_control"`
}

// note reports whether the block is a breakpoint and records its tier.
func (c *cacheBreakpoints) note(p cacheControlProbe) bool {
	if p.CacheControl == nil {
		return false
	}
	// canonical: per-breakpoint ttl dropped — canonical TTL is request-wide, so one 1h breakpoint lifts every re-emitted breakpoint to 1h.
	if p.CacheControl.TTL == "1h" {
		c.longTTL = true
	}
	return true
}

// blockMarked reports whether a single block (a tool definition) carries a breakpoint.
func (c *cacheBreakpoints) blockMarked(raw json.RawMessage) bool {
	var p cacheControlProbe
	return json.Unmarshal(raw, &p) == nil && c.note(p)
}

// systemMarked reports whether an array-form system prompt carries a breakpoint; the string form cannot.
func (c *cacheBreakpoints) systemMarked(raw json.RawMessage) bool {
	var blocks []cacheControlProbe
	if json.Unmarshal(raw, &blocks) != nil {
		return false
	}
	marked := false
	for _, b := range blocks {
		if c.note(b) {
			marked = true
		}
	}
	return marked
}

// contentAnchor returns the item anchor for a message whose content blocks carry a breakpoint.
func (c *cacheBreakpoints) contentAnchor(content json.RawMessage) *v1.ItemCacheConfig {
	var blocks []cacheControlProbe
	if len(content) == 0 || content[0] != '[' || json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var anchor *v1.ItemCacheConfig
	for _, b := range blocks {
		// canonical: cache_control on tool_result, tool_use and thinking blocks dropped — those blocks parse to items other than Message, and only Message carries an ItemCacheConfig anchor.
		switch b.Type {
		case "text", "image", "document":
			if c.note(b) {
				anchor = &v1.ItemCacheConfig{Anchor: true}
			}
		}
	}
	return anchor
}

// config returns the request-level CacheConfig, nil when no breakpoint maps to one.
func (c *cacheBreakpoints) config(instructions, tools bool) *v1.CacheConfig {
	if !instructions && !tools && !c.longTTL {
		return nil
	}
	cfg := &v1.CacheConfig{Instructions: instructions, Tools: tools}
	if c.longTTL {
		cfg.TTL = "1h"
	}
	return cfg
}
