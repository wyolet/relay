package anthropic

// anthropicMaxBreakpoints is Anthropic's per-request cap on cache_control blocks; one more is a 400.
const anthropicMaxBreakpoints = 4

// markHistoryBreakpoint puts a breakpoint on the last block of the last message, so the whole input is the cached prefix. Thinking blocks cannot carry cache_control, so the mark lands on the nearest block before them.
func markHistoryBreakpoint(msgs []anthropicCanonMsg, ttl string) {
	if len(msgs) == 0 {
		return
	}
	last := &msgs[len(msgs)-1]
	switch c := last.Content.(type) {
	case string:
		last.Content = withCacheBreakpoint(c, ttl)
	case []map[string]any:
		for i := len(c) - 1; i >= 0; i-- {
			if t := c[i]["type"]; t == "thinking" || t == "redacted_thinking" {
				continue
			}
			c[i]["cache_control"] = anthropicEphemeralCacheControl(ttl)
			return
		}
	}
}

// trimMessageBreakpoints keeps message breakpoints within the cap left after the fixed ones (tools, system). The latest breakpoints stay: each covers a longer prefix than any before it, and the history breakpoint is always the last.
func trimMessageBreakpoints(msgs []anthropicCanonMsg, fixed int) {
	var marked []map[string]any
	for _, m := range msgs {
		blocks, ok := m.Content.([]map[string]any)
		if !ok {
			continue
		}
		for _, b := range blocks {
			if _, ok := b["cache_control"]; ok {
				marked = append(marked, b)
			}
		}
	}
	excess := fixed + len(marked) - anthropicMaxBreakpoints
	for i := 0; i < excess && i < len(marked); i++ {
		// canonical: oldest ItemCacheConfig anchors dropped — Anthropic caps a request at 4 breakpoints, and a later anchor already covers the dropped one's prefix.
		delete(marked[i], "cache_control")
	}
}
