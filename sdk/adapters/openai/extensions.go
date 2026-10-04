package openai

import (
	"encoding/json"
	"fmt"
)

// Extensions keys owned by this package. Each carries an OpenAI wire knob with
// no cross-vendor intent (rule 7) under its wire name; adapters that don't own
// the "openai." prefix ignore them.
const (
	extLogitBias   = "openai.logit_bias"
	extLogprobs    = "openai.logprobs" // request: bool; response: the choice logprobs object
	extTopLogprobs = "openai.top_logprobs"
	extServiceTier = "openai.service_tier"
	extStore       = "openai.store"
)

// ccExtensionsFromWire carries the CC request knobs canonical has no field for.
func ccExtensionsFromWire(w *FullChatRequest) map[string]json.RawMessage {
	ext := map[string]json.RawMessage{}
	put := func(key string, v any) {
		if b, err := json.Marshal(v); err == nil {
			ext[key] = b
		}
	}
	if len(w.LogitBias) > 0 {
		put(extLogitBias, w.LogitBias)
	}
	if w.Logprobs != nil {
		put(extLogprobs, *w.Logprobs)
	}
	if w.TopLogprobs != nil {
		put(extTopLogprobs, *w.TopLogprobs)
	}
	if w.ServiceTier != "" {
		put(extServiceTier, w.ServiceTier)
	}
	if w.Store != nil {
		put(extStore, *w.Store)
	}
	if len(ext) == 0 {
		return nil
	}
	return ext
}

// ccApplyExtensions writes the CC-owned extension keys onto the wire request.
// A malformed value under an owned key is the caller's error, not a drop.
func ccApplyExtensions(out *FullChatRequest, ext map[string]json.RawMessage) error {
	targets := map[string]any{
		extLogitBias:   &out.LogitBias,
		extLogprobs:    &out.Logprobs,
		extTopLogprobs: &out.TopLogprobs,
		extServiceTier: &out.ServiceTier,
		extStore:       &out.Store,
	}
	for key, dst := range targets {
		raw, ok := ext[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("extensions[%q]: %w", key, err)
		}
	}
	return nil
}
