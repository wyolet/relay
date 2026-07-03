package openai

import (
	"time"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// openaiCacheRetention maps canonical CacheConfig.TTL to OpenAI's
// prompt_cache_retention tier. OpenAI offers exactly two: "in_memory"
// (~5–10 min) and "24h" (KV offload to GPU-local storage). Anything beyond
// the in-memory window upgrades to "24h"; unset/unparseable emits nothing
// (vendor default). Shared by the CC and Responses translators.
func openaiCacheRetention(cfg *v1.CacheConfig) string {
	d, ok := cfg.TTLDuration()
	if !ok {
		return ""
	}
	if d > 10*time.Minute {
		return "24h"
	}
	return "in_memory"
}

// openaiRetentionTTL maps an inbound prompt_cache_retention value to the
// canonical TTL hint. "in_memory" becomes "5m" (round-trips back to
// "in_memory" and lands on other vendors' short tier); unknown values map
// to unset.
func openaiRetentionTTL(retention string) string {
	switch retention {
	case "24h":
		return "24h"
	case "in_memory":
		return "5m"
	default:
		return ""
	}
}

// openaiCacheConfigFromWire builds the canonical CacheConfig for an inbound
// OpenAI-shaped request from its cache fields, or nil when both are unset.
func openaiCacheConfigFromWire(promptCacheKey, promptCacheRetention string) *v1.CacheConfig {
	ttl := openaiRetentionTTL(promptCacheRetention)
	if promptCacheKey == "" && ttl == "" {
		return nil
	}
	return &v1.CacheConfig{Key: promptCacheKey, TTL: ttl}
}
