// Package litellm implements the "relay catalog import litellm" subcommand.
// It fetches LiteLLM's MIT-licensed model_prices_and_context_window.json,
// translates chat-mode entries into Wyolet catalog entities, and writes them
// via the catalog storage layer.
//
// I/O boundary: Fetch and Apply are the only I/O functions.
// Translate is pure and deterministic.
package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// DefaultLiteLLMURL is the canonical upstream source for model pricing data.
const DefaultLiteLLMURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// Entry is a single entry from LiteLLM's model_prices_and_context_window.json.
// Fields are sparse; all are optional except those checked by Translate.
type Entry struct {
	LiteLLMProvider string `json:"litellm_provider"`
	Mode            string `json:"mode"` // "chat", "embedding", "image_generation", ...

	MaxTokens      int `json:"max_tokens"`
	MaxInputTokens int `json:"max_input_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`

	InputCostPerToken             float64 `json:"input_cost_per_token"`
	OutputCostPerToken            float64 `json:"output_cost_per_token"`
	CacheCreationInputTokenCost   float64 `json:"cache_creation_input_token_cost"`
	CacheReadInputTokenCost       float64 `json:"cache_read_input_token_cost"`
	OutputCostPerReasoningToken   float64 `json:"output_cost_per_reasoning_token"`
	InputCostPerTokenBatches      float64 `json:"input_cost_per_token_batches"`
	OutputCostPerTokenBatches     float64 `json:"output_cost_per_token_batches"`
	InputCostPerImage             float64 `json:"input_cost_per_image"`
	InputCostPerAudioToken        float64 `json:"input_cost_per_audio_token"`
	OutputCostPerAudioToken       float64 `json:"output_cost_per_audio_token"`

	DeprecationDate string `json:"deprecation_date"`

	SupportsFunctionCalling         bool `json:"supports_function_calling"`
	SupportsParallelFunctionCalling bool `json:"supports_parallel_function_calling"`
	SupportsVision                  bool `json:"supports_vision"`
	SupportsPromptCaching           bool `json:"supports_prompt_caching"`
	SupportsReasoning               bool `json:"supports_reasoning"`
	SupportsResponseSchema          bool `json:"supports_response_schema"`
	SupportsNativeStructuredOutput  bool `json:"supports_native_structured_output"`
	SupportsSystemMessages          bool `json:"supports_system_messages"`
	SupportsAssistantPrefill        bool `json:"supports_assistant_prefill"`
	SupportsPDFInput                bool `json:"supports_pdf_input"`
	SupportsAudioInput              bool `json:"supports_audio_input"`
	SupportsAudioOutput             bool `json:"supports_audio_output"`
	SupportsWebSearch               bool `json:"supports_web_search"`
	SupportsComputerUse             bool `json:"supports_computer_use"`
	SupportsNativeStreaming          bool `json:"supports_native_streaming"`

	// SupportedEndpoints is used to infer batch support.
	SupportedEndpoints []string `json:"supported_endpoints"`
}

// Fetch retrieves the LiteLLM JSON from url, parses it, and returns the model
// map and a version string (best-effort git SHA from headers, else today's date).
// Use sourceFile to read from a local file instead of making an HTTP request.
func Fetch(ctx context.Context, url, sourceFile string) (map[string]Entry, string, error) {
	var body []byte

	if sourceFile != "" {
		b, err := os.ReadFile(sourceFile)
		if err != nil {
			return nil, "", fmt.Errorf("fetch: read file %q: %w", sourceFile, err)
		}
		body = b
	} else {
		httpCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(httpCtx, http.MethodGet, url, nil)
		if err != nil {
			return nil, "", fmt.Errorf("fetch: build request: %w", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("fetch: GET %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("fetch: GET %s: HTTP %d", url, resp.StatusCode)
		}
		body, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, "", fmt.Errorf("fetch: read body: %w", err)
		}
	}

	// LiteLLM's JSON is a flat object where each key is a model name and the
	// value is the entry. There is a special "sample_spec" key to drop.
	raw := make(map[string]json.RawMessage)
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", fmt.Errorf("fetch: parse JSON: %w", err)
	}
	delete(raw, "sample_spec")

	entries := make(map[string]Entry, len(raw))
	for k, v := range raw {
		var e Entry
		if err := json.Unmarshal(v, &e); err != nil {
			// Skip malformed entries; they shouldn't break the whole import.
			continue
		}
		entries[k] = e
	}

	version := time.Now().Format("2006-01-02")
	return entries, version, nil
}
