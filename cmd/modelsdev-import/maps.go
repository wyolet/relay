package main

import (
	"regexp"
	"strings"
)

// adapterForNPM maps a models.dev `npm` (@ai-sdk/*) tag to a relay adapter.
// The overwhelming majority are OpenAI Chat Completions on the wire — every
// branded wrapper (groq, xai, perplexity, together, deepinfra, cerebras) and
// `@ai-sdk/openai-compatible` collapse to "openai". Returns ok=false for wire
// shapes relay has no adapter for yet (Cohere, Bedrock Converse) so the
// caller can route them to drafts/.
func adapterForNPM(npm string) (string, bool) {
	switch npm {
	case "@ai-sdk/anthropic", "@ai-sdk/google-vertex/anthropic":
		return "anthropic", true
	case "@ai-sdk/google", "@ai-sdk/google-vertex":
		return "gemini", true
	case "@ai-sdk/cohere":
		return "", false // no Cohere adapter yet
	case "@ai-sdk/amazon-bedrock":
		return "", false // no Bedrock Converse adapter yet
	}
	// openai, azure, openai-compatible, and every branded/gateway wrapper.
	return "openai", true
}

// meterFor translates a models.dev cost key to a relay pricing meter. The
// closure test (TestMetersAreKnown) guards that every emitted meter is one
// the cost engine understands — a missing entry here is a silent under-bill.
var meterFor = map[string]string{
	"input":        "tokens.input",
	"output":       "tokens.output",
	"cache_read":   "tokens.cache_read",
	"cache_write":  "tokens.cache_creation",
	"reasoning":    "tokens.reasoning",
	"input_audio":  "tokens.audio_input",
	"output_audio": "tokens.audio_output",
}

// knownBaseURL supplies a baseURL for first-party providers that omit `api`
// in models.dev (the AI SDK hardcodes their endpoints). Providers absent
// here AND without an `api` field can't form a Host and are skipped.
var knownBaseURL = map[string]string{
	"anthropic":  "https://api.anthropic.com",
	"openai":     "https://api.openai.com",
	"google":     "https://generativelanguage.googleapis.com",
	"groq":       "https://api.groq.com/openai/v1",
	"deepseek":   "https://api.deepseek.com",
	"xai":        "https://api.x.ai/v1",
	"mistral":    "https://api.mistral.ai/v1",
	"cerebras":   "https://api.cerebras.ai/v1",
	"perplexity": "https://api.perplexity.ai",
}

// baseURLFor resolves a host baseURL: provider.api wins, then the known
// table. Trailing slash trimmed for consistency with hand-curated hosts.
func baseURLFor(p MDProvider) (string, bool) {
	if p.API != "" {
		return strings.TrimRight(p.API, "/"), true
	}
	if u, ok := knownBaseURL[p.ID]; ok {
		return u, true
	}
	return "", false
}

// pricingStrategiesFor returns the billing-mode menu for a host. Presence of
// per-token cost in models.dev means "api" is always available; "sub" is a
// known-host fact models.dev doesn't carry, so it's a small curated overlay.
// Hosts whose models are NOT per-token billable (subscription-only) return
// just ["sub"].
func pricingStrategiesFor(providerID string) []string {
	switch providerID {
	case "ollama-cloud":
		return []string{"sub"} // flat plan only, no per-token billing
	case "anthropic":
		return []string{"api", "sub"} // API key or Claude Max subscription
	}
	return []string{"api"}
}

// dateOrSeqSuffix matches a trailing dated/sequence/alias variant suffix on a
// slug: compact dates (20251001), hyphenated dates (2024-08-06), bare years/
// sequence numbers (2411, 001), or "latest". Used to fold variants of the
// same model into one Model with multiple snapshots (the catalog convention),
// rather than emitting each as its own model. Letter-suffixed sizes ("72b",
// "8x7b") are NOT matched, so semantic variants (mini, pro, 72b) never fold.
var dateOrSeqSuffix = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{8}|\d{6}|\d{4}|\d{3}|latest)$`)

// foldKey returns the base model slug a variant folds into. Idempotent for
// already-base slugs.
func foldKey(slug string) string {
	if base := dateOrSeqSuffix.ReplaceAllString(slug, ""); base != "" {
		return base
	}
	return slug
}

var slugBad = regexp.MustCompile(`[^a-z0-9]+`)

// slugify normalizes a models.dev key/name to a DNS-1123 relay slug. Lossy by
// design: the exact wire string is preserved separately as the binding's
// UpstreamName, so collapsing "." "/" ":" "@" here is safe.
func slugify(s string) string {
	s = strings.ToLower(s)
	s = slugBad.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}
