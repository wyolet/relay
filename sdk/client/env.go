package client

import (
	"fmt"
	"os"
	"strings"
)

// Relay env vars, mirroring the OpenAI SDK's OPENAI_BASE_URL / OPENAI_API_KEY
// convention. Only the relay target consults them — direct-to-vendor clients
// do not, so an upstream call never depends on relay config.
//
//	WR_BASE_URL  relay endpoint           (fallback when baseURL == "")
//	WR_API_KEY   relay key                (fallback when relayKey == "")
//	WR_USAGE     default X-WR-Usage value  (e.g. "full" to echo usage inline)
//	WR_HEADERS   default headers, "k1=v1,k2=v2"
//	WR_TIMEOUT   sync-call timeout, a Go duration (e.g. "30s"); streams are
//	             unaffected — a stream needs no overall deadline.
const (
	EnvBaseURL = "WR_BASE_URL"
	EnvAPIKey  = "WR_API_KEY"
	EnvUsage   = "WR_USAGE"
	EnvHeaders = "WR_HEADERS"
	EnvTimeout = "WR_TIMEOUT"
)

// headerUsage is httpheader.HeaderUsage, inlined: the client deliberately
// imports nothing of relay's server graph (see package doc).
const headerUsage = "X-WR-Usage"

// Conventional per-vendor key env vars, read when a direct-to-vendor
// constructor is called with an empty apiKey. These are the vendor's own
// credential — never WR_API_KEY, which is a relay key bound to a relay
// endpoint and meaningless to a vendor. An empty key after fallback is left
// as-is (Ollama and other keyless OpenAI-compatible hosts are valid).
const (
	EnvOpenAIKey    = "OPENAI_API_KEY"
	EnvAnthropicKey = "ANTHROPIC_API_KEY"
	// Gemini reads GEMINI_API_KEY first, then GOOGLE_API_KEY — matching the
	// google-genai SDK precedence.
	EnvGeminiKey = "GEMINI_API_KEY"
	EnvGoogleKey = "GOOGLE_API_KEY"
)

// relayEnvOptions builds the Options implied by WR_USAGE and WR_HEADERS.
func relayEnvOptions() ([]Option, error) {
	var opts []Option
	if u := os.Getenv(EnvUsage); u != "" {
		opts = append(opts, WithHeader(headerUsage, u))
	}
	hdrs, err := parseHeaderEnv(os.Getenv(EnvHeaders))
	for k, v := range hdrs {
		opts = append(opts, WithHeader(k, v))
	}
	return opts, err
}

// parseHeaderEnv parses "k1=v1,k2=v2" into a header map. An entry without an
// "=" or with an empty key is an error (no silent drop) — it surfaces on the
// first call like any other config problem.
func parseHeaderEnv(s string) (map[string]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if strings.TrimSpace(pair) == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("relay client: invalid %s entry %q (want k=v)", EnvHeaders, pair)
		}
		out[k] = strings.TrimSpace(v)
	}
	return out, nil
}
