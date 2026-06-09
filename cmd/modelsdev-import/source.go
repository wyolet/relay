package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// DefaultModelsDevURL is the canonical models.dev dataset.
const DefaultModelsDevURL = "https://models.dev/api.json"

// MDProvider is one provider object in models.dev api.json. The top-level
// document is map[providerID]MDProvider.
type MDProvider struct {
	ID     string             `json:"id"`
	Name   string             `json:"name"`
	Env    []string           `json:"env"`
	NPM    string             `json:"npm"` // the @ai-sdk/* tag → our adapter
	Doc    string             `json:"doc"`
	API    string             `json:"api"` // baseURL; absent for first-party SDK-known providers
	Models map[string]MDModel `json:"models"`
}

// MDModel is one model entry. We capture every field models.dev carries so
// nothing is silently dropped; unmapped-but-useful bits land in labels/tags.
type MDModel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Family      string `json:"family"`
	Attachment  bool   `json:"attachment"`
	Reasoning   bool   `json:"reasoning"`
	ToolCall    bool   `json:"tool_call"`
	Temperature bool   `json:"temperature"`
	Knowledge   string `json:"knowledge"`
	ReleaseDate string `json:"release_date"`
	LastUpdated string `json:"last_updated"`
	OpenWeights bool   `json:"open_weights"`
	Modalities  struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
		Input   int `json:"input"`
	} `json:"limit"`
	Cost          MDCost          `json:"cost"`
	ReasoningOpts json.RawMessage `json:"reasoning_options"`
}

// MDCost is the per-model cost block. Pointers distinguish "absent" from
// "zero" so a free model's explicit 0 still emits a rate.
type MDCost struct {
	Input       *float64 `json:"input"`
	Output      *float64 `json:"output"`
	CacheRead   *float64 `json:"cache_read"`
	CacheWrite  *float64 `json:"cache_write"`
	Reasoning   *float64 `json:"reasoning"`
	InputAudio  *float64 `json:"input_audio"`
	OutputAudio *float64 `json:"output_audio"`
	Tiers       []MDTier `json:"tiers"`
	// ContextOver200k is models.dev's older single-threshold tier form; we
	// prefer Tiers (it carries the size) and fall back to a 200k threshold.
	ContextOver200k *MDTierRates `json:"context_over_200k"`
}

// MDTier is one volume tier: rates that apply above a context size.
type MDTier struct {
	MDTierRates
	Tier struct {
		Type string `json:"type"` // "context"
		Size int    `json:"size"` // token threshold → aboveTokens
	} `json:"tier"`
}

// MDTierRates holds the per-meter amounts for a tier (or the base block).
type MDTierRates struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

// Fetch loads the models.dev dataset from a URL or, if file != "", a local
// file. Returns providers sorted by id for deterministic output.
func Fetch(ctx context.Context, url, file string) ([]MDProvider, error) {
	var raw []byte
	var err error
	if file != "" {
		raw, err = os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
	} else {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("fetch %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("fetch %s: status %d", url, resp.StatusCode)
		}
		raw, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
	}

	var doc map[string]MDProvider
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	out := make([]MDProvider, 0, len(doc))
	for id, p := range doc {
		if p.ID == "" {
			p.ID = id
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// SourceVersion stamps imported rows with the import date (models.dev has no
// dataset version field).
func SourceVersion() string { return time.Now().UTC().Format("2006-01-02") }

// sortedModelIDs returns a provider's model keys in stable order.
func sortedModelIDs(p MDProvider) []string {
	ids := make([]string, 0, len(p.Models))
	for k := range p.Models {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	return ids
}

var _ = strings.TrimSpace
