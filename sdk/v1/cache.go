package v1

// CacheConfig is the vendor-neutral prompt-cache configuration for a request.
//
// It declares *intent* only: which stable prefixes of the request the caller
// considers worth caching. There is deliberately no per-vendor cache vocabulary
// here — no "cache_control", no "prompt_cache_key", no ephemeral/persistent
// distinction. Each flag means "this section is a stable prefix; cache it if
// the upstream can." A client sets identical values regardless of which vendor
// the request is routed to.
//
// Adapter contract:
//   - Vendors with explicit cache control (Anthropic) translate each enabled
//     flag into the corresponding wire breakpoint.
//   - Vendors that cache automatically or expose no breakpoint API (OpenAI)
//     ignore CacheConfig entirely — it is a no-op, never an error.
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
