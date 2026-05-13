// Package main implements the litellm-import CLI for the new app/ architecture.
// It fetches LiteLLM's model_prices_and_context_window.json and emits YAML files
// using app/manifest DTO types.
package main

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
type Entry struct {
	LiteLLMProvider string `json:"litellm_provider"`
	Mode            string `json:"mode"` // "chat", "embedding", ...

	MaxTokens       int `json:"max_tokens"`
	MaxInputTokens  int `json:"max_input_tokens"`
	MaxOutputTokens int `json:"max_output_tokens"`

	InputCostPerToken           float64 `json:"input_cost_per_token"`
	OutputCostPerToken          float64 `json:"output_cost_per_token"`
	CacheCreationInputTokenCost float64 `json:"cache_creation_input_token_cost"`
	CacheReadInputTokenCost     float64 `json:"cache_read_input_token_cost"`
	OutputCostPerReasoningToken float64 `json:"output_cost_per_reasoning_token"`

	InputCostPerTokenAbove128kTokens  float64 `json:"input_cost_per_token_above_128k_tokens"`
	OutputCostPerTokenAbove128kTokens float64 `json:"output_cost_per_token_above_128k_tokens"`
	InputCostPerTokenAbove200kTokens  float64 `json:"input_cost_per_token_above_200k_tokens"`
	OutputCostPerTokenAbove200kTokens float64 `json:"output_cost_per_token_above_200k_tokens"`
	InputCostPerTokenAbove272kTokens  float64 `json:"input_cost_per_token_above_272k_tokens"`
	OutputCostPerTokenAbove272kTokens float64 `json:"output_cost_per_token_above_272k_tokens"`

	InputCostPerTokenBatches    float64 `json:"input_cost_per_token_batches"`
	OutputCostPerTokenBatches   float64 `json:"output_cost_per_token_batches"`
	InputCostPerImage           float64 `json:"input_cost_per_image"`
	InputCostPerAudioToken      float64 `json:"input_cost_per_audio_token"`
	OutputCostPerAudioToken     float64 `json:"output_cost_per_audio_token"`

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
	SupportsNativeStreaming         bool `json:"supports_native_streaming"`

	SupportedEndpoints []string `json:"supported_endpoints"`
}

// Fetch retrieves the LiteLLM JSON from url (or sourceFile if non-empty),
// parses it, and returns the model map plus a version string.
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

	raw := make(map[string]json.RawMessage)
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", fmt.Errorf("fetch: parse JSON: %w", err)
	}
	delete(raw, "sample_spec")

	entries := make(map[string]Entry, len(raw))
	for k, v := range raw {
		var e Entry
		if err := json.Unmarshal(v, &e); err != nil {
			continue
		}
		entries[k] = e
	}

	version := time.Now().Format("2006-01-02")
	return entries, version, nil
}
