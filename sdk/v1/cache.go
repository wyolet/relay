package v1

import "time"

// CacheConfig is the vendor-neutral prompt-cache configuration for a request.
//
// It declares *intent* only: which stable prefixes of the request the caller
// considers worth caching (Instructions/Tools), which requests share a prefix
// lineage (Key), and how long the prefix should stay reusable (TTL). There is
// deliberately no per-vendor cache vocabulary here — no "cache_control", no
// ephemeral/persistent distinction. A client sets identical values regardless
// of which vendor the request is routed to.
//
// Adapter contract:
//   - Vendors with explicit cache control (Anthropic) translate each enabled
//     flag into the corresponding wire breakpoint and clamp TTL to their
//     retention tiers; Key is ignored (their caching needs no routing hint).
//   - Vendors that cache automatically with shard routing (OpenAI) ignore
//     the flags and emit Key as the wire cache key (prompt_cache_key) and
//     TTL as the retention tier (prompt_cache_retention).
//   - Vendors with neither (Gemini implicit cache) ignore CacheConfig
//     entirely — it is a no-op, never an error.
//
// Semantics are cumulative in the vendor's own prefix order: an enabled flag
// means "everything up to and including this section is stable." Whether
// reads hit is observable via Response.Usage["cache_read"] /
// ["cache_creation"], which adapters already normalize across vendors.
//
// Breakpoint budget: vendors cap explicit breakpoints (Anthropic allows 4).
// The flags here stay well within that bound; future per-Item history anchors
// (the "system_and_N" pattern) will share the same budget and are an additive
// extension, not a breaking change.
type CacheConfig struct {
	// Instructions requests caching of the system/instructions prefix.
	Instructions bool `json:"instructions,omitempty"`
	// Tools requests caching of the tools block.
	Tools bool `json:"tools,omitempty"`
	// Key is an optional stable cache identity for the request's prefix
	// lineage — typically a conversation or session id, identical across
	// turns. It declares "requests sharing this key share a cacheable
	// prefix; keep them cache-local." Vendors that route to cache shards
	// per request (OpenAI: prompt_cache_key) combine it with the prefix
	// hash so same-key requests land on the same shard — without it,
	// routing is best-effort and long conversations take random total
	// cache misses. Vendors whose caching is deterministic (Anthropic
	// breakpoints) or keyless (Gemini implicit) ignore it. Keep the key
	// coarse enough that one key stays under the vendor's per-shard rate
	// (OpenAI: ~15 req/min per prefix+key).
	Key string `json:"key,omitempty"`
	// TTL is how long the cached prefix should stay reusable between
	// requests, as a Go duration string ("5m", "1h", "24h"). It is a hint:
	// each vendor clamps to its nearest supported retention tier —
	// Anthropic breakpoints upgrade to their 1-hour tier when TTL exceeds
	// the default 5 minutes (note: 1h writes cost 2× vs 1.25×); OpenAI maps
	// TTL over ~10 minutes to prompt_cache_retention "24h", at or under to
	// "in_memory". Unset means the vendor default. Unparseable values are
	// treated as unset.
	TTL string `json:"ttl,omitempty"`
}

// TTLDuration parses TTL. ok is false when TTL is unset or unparseable —
// adapters treat both as "vendor default", never an error.
func (c *CacheConfig) TTLDuration() (time.Duration, bool) {
	if c == nil || c.TTL == "" {
		return 0, false
	}
	d, err := time.ParseDuration(c.TTL)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// ItemCacheConfig is the per-item analogue of CacheConfig, carried on an input
// item (cache_config). It marks the item as a cache anchor: everything up to
// and including this item is a stable prefix. Same neutrality contract —
// supporting adapters emit a breakpoint at this item, others ignore it. Used
// for the rolling "stable history up to here" anchor as a conversation grows
// (the system_and_N pattern). Counts against the vendor breakpoint budget
// alongside the request-level CacheConfig flags.
type ItemCacheConfig struct {
	// Anchor marks this item as a cache breakpoint.
	Anchor bool `json:"anchor,omitempty"`
}
