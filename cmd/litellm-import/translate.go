package main

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
)

// providerMeta holds static display + endpoint metadata per known litellm_provider.
// adapter is the wire protocol the relay must speak to this provider's host(s).
type providerMeta struct {
	name          string
	displayName   string
	description   string
	baseURL       string
	adapter       string // adapter.Kind value: "openai" or "anthropic"
	homepageURL   string
	docsURL       string
	consoleURL    string
	statusPageURL string
	iconPath      string
}

var knownProviders = map[string]providerMeta{
	"anthropic": {
		name:          "anthropic",
		displayName:   "Anthropic",
		description:   "Claude models from Anthropic.",
		baseURL:       "https://api.anthropic.com",
		adapter:       "anthropic",
		homepageURL:   "https://www.anthropic.com",
		docsURL:       "https://docs.anthropic.com",
		consoleURL:    "https://console.anthropic.com",
		statusPageURL: "https://status.anthropic.com",
		iconPath:      "/provider/anthropic.svg",
	},
	"openai": {
		name:          "openai",
		displayName:   "OpenAI",
		description:   "GPT models from OpenAI.",
		baseURL:       "https://api.openai.com",
		adapter:       "openai",
		homepageURL:   "https://openai.com",
		docsURL:       "https://platform.openai.com/docs",
		consoleURL:    "https://platform.openai.com",
		statusPageURL: "https://status.openai.com",
		iconPath:      "/provider/openai.svg",
	},
	"ollama": {
		name:        "ollama",
		displayName: "Ollama",
		description: "Run large language models locally with Ollama.",
		baseURL:     "http://host.docker.internal:11434",
		adapter:     "openai", // Ollama exposes OpenAI-compatible endpoint
		homepageURL: "https://ollama.com",
		iconPath:    "/provider/ollama.png",
	},
}

// skippedProviders is intentionally empty after the v1alpha2 reimport —
// the catalog now ingests every LiteLLM provider so future curation can
// happen by hand rather than another importer run.
var skippedProviders = map[string]string{}

// titleCase turns "fireworks_ai" → "Fireworks Ai" for synthesized displayName.
func titleCase(s string) string {
	parts := strings.Split(strings.ReplaceAll(s, "-", "_"), "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// fallbackProvider synthesizes a minimal providerMeta for any litellm_provider
// not in knownProviders. Adapter defaults to "openai" (the most common
// compat shape); the catalog author fixes baseURL + adapter by hand.
func fallbackProvider(name string) providerMeta {
	display := titleCase(name)
	return providerMeta{
		name:        name,
		displayName: display,
		description: display + " — auto-imported, requires hand curation.",
		baseURL:     "",
		adapter:     "openai",
	}
}

// Matches both compact (-YYYYMMDD, Anthropic-style) and dashed
// (-YYYY-MM-DD, OpenAI-style) date suffixes.
var dateSuffixRE = regexp.MustCompile(`-(\d{4})-?(\d{2})-?(\d{2})$`)
var suffixesToStrip = []string{"-latest", "-preview", "-current"}

func baseName(name string) string {
	n := dateSuffixRE.ReplaceAllString(name, "")
	for _, s := range suffixesToStrip {
		n = strings.TrimSuffix(n, s)
	}
	return n
}

// extractDateSuffix returns the date in compact YYYYMMDD form, regardless
// of which separator style the source name used.
func extractDateSuffix(name string) string {
	m := dateSuffixRE.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	return m[1] + m[2] + m[3]
}

// snapshotEntry is one dated checkpoint within a Group.
type snapshotEntry struct {
	Name         string // litellm entry name (e.g. gpt-4o-2024-08-06)
	OriginalName string // what we send upstream (same as Name today)
	ReleasedAt   string // YYYY-MM-DD parsed from the date suffix, if any
}

// Group is one Model worth of LiteLLM entries: a stable Model name, the
// dated Snapshots that live under it, the snapshot the bare name resolves
// to, optional moving-label aliases (-latest, -preview, ...), and the
// canonical entry whose capabilities/pricing represent the group.
type Group struct {
	ModelName string
	Snapshots []snapshotEntry
	Pointer   string
	Aliases   []string
	Entry     Entry
}

var movingLabelRE = regexp.MustCompile(`-(latest|preview|current)$`)

// CollapseAliases groups entries by base name. Each dated entry becomes a
// Snapshot; -latest/-preview/-current become Model aliases; if no dated
// entries exist a default Snapshot named after the model is synthesized.
// On capability/pricing conflict, dated members are emitted as separate
// single-snapshot Models.
func CollapseAliases(entries map[string]Entry) []Group {
	byBase := map[string][]string{}
	for name := range entries {
		b := baseName(name)
		byBase[b] = append(byBase[b], name)
	}

	var groups []Group
	for base, names := range byBase {
		sort.Strings(names)

		var dated, moving []string
		bareExists := false
		for _, n := range names {
			switch {
			case extractDateSuffix(n) != "":
				dated = append(dated, n)
			case movingLabelRE.MatchString(n):
				moving = append(moving, n)
			case n == base:
				bareExists = true
			default:
				moving = append(moving, n) // unrecognized → treat as alias
			}
		}

		// Pick the canonical entry: latest dated, else bare, else any.
		canonical := ""
		if len(dated) > 0 {
			canonical = dated[len(dated)-1] // sorted ascending → last is newest
		} else if bareExists {
			canonical = base
		} else {
			canonical = names[0]
		}
		canonicalEntry := entries[canonical]

		// Conflict check across dated members. If any conflict, fall back
		// to emitting each dated entry as its own single-snapshot Model.
		hasConflict := false
		for _, n := range dated {
			if n == canonical {
				continue
			}
			if entriesConflict(canonicalEntry, entries[n]) {
				slog.Warn("litellm-import: snapshot conflict — emitting separately", "model", base, "snapshot", n)
				hasConflict = true
			}
		}
		if hasConflict {
			for _, n := range dated {
				groups = append(groups, Group{
					ModelName: n,
					Snapshots: []snapshotEntry{{Name: n, OriginalName: n, ReleasedAt: parseReleasedAt(n)}},
					Pointer:   n,
					Entry:     entries[n],
				})
			}
			continue
		}

		var snaps []snapshotEntry
		for _, n := range dated {
			snaps = append(snaps, snapshotEntry{Name: n, OriginalName: n, ReleasedAt: parseReleasedAt(n)})
		}
		pointer := ""
		if len(snaps) > 0 {
			pointer = snaps[len(snaps)-1].Name // newest
		} else {
			snaps = []snapshotEntry{{Name: base, OriginalName: base}}
			pointer = base
		}
		groups = append(groups, Group{
			ModelName: base,
			Snapshots: snaps,
			Pointer:   pointer,
			Aliases:   moving,
			Entry:     canonicalEntry,
		})
	}

	sort.Slice(groups, func(i, j int) bool { return groups[i].ModelName < groups[j].ModelName })
	return groups
}

// parseReleasedAt turns the LiteLLM date suffix (YYYYMMDD) into ISO YYYY-MM-DD.
func parseReleasedAt(name string) string {
	s := extractDateSuffix(name)
	if len(s) != 8 {
		return ""
	}
	return s[:4] + "-" + s[4:6] + "-" + s[6:8]
}

func entriesConflict(a, b Entry) bool {
	for _, pair := range [][2]float64{{a.InputCostPerToken, b.InputCostPerToken}, {a.OutputCostPerToken, b.OutputCostPerToken}} {
		if pair[0] != 0 && pair[1] != 0 && pair[0] != pair[1] {
			return true
		}
	}
	return a.SupportsFunctionCalling != b.SupportsFunctionCalling ||
		a.SupportsVision != b.SupportsVision ||
		a.SupportsReasoning != b.SupportsReasoning ||
		a.MaxInputTokens != b.MaxInputTokens
}

func inferFamily(name string) string {
	switch {
	case strings.HasPrefix(name, "claude-"):
		return "claude"
	case strings.HasPrefix(name, "gpt-"):
		return "gpt"
	case strings.HasPrefix(name, "o1-") || strings.HasPrefix(name, "o3-") || strings.HasPrefix(name, "o4-"):
		return "o-series"
	case strings.HasPrefix(name, "gemini-"):
		return "gemini"
	default:
		return strings.SplitN(name, "-", 2)[0]
	}
}

func inferModalities(e Entry) model.Modalities {
	inputs := []string{"text"}
	if e.SupportsVision {
		inputs = append(inputs, "image")
	}
	if e.SupportsAudioInput {
		inputs = append(inputs, "audio")
	}
	if e.SupportsPDFInput {
		inputs = append(inputs, "file")
	}
	outputs := []string{"text"}
	if e.SupportsAudioOutput {
		outputs = append(outputs, "audio")
	}
	return model.Modalities{Input: inputs, Output: outputs}
}

func hasBatchEndpoint(e Entry) bool {
	for _, ep := range e.SupportedEndpoints {
		if strings.Contains(ep, "batch") {
			return true
		}
	}
	return false
}

func deprecationInfo(e Entry, today time.Time) *model.Deprecation {
	if e.DeprecationDate == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", e.DeprecationDate)
	if err != nil {
		return nil
	}
	if t.Before(today) {
		return &model.Deprecation{Status: model.DeprecationSunset, SunsetDate: e.DeprecationDate}
	}
	return &model.Deprecation{Status: model.DeprecationDeprecated, SunsetDate: e.DeprecationDate}
}

// pricingRate is one row in a Pricing YAML spec.rates list.
type pricingRate struct {
	Meter       string  `yaml:"meter"`
	Unit        string  `yaml:"unit"`
	Amount      float64 `yaml:"amount"`
	AboveTokens int     `yaml:"aboveTokens,omitempty"`
}

// pricingSpec is the spec block of a Pricing YAML document.
type pricingSpec struct {
	Currency     string        `yaml:"currency"`
	TargetModels []string      `yaml:"targetModels"`
	Rates        []pricingRate `yaml:"rates"`
}

// pricingMeta is the metadata block of a Pricing YAML document.
type pricingMeta struct {
	Name  string             `yaml:"name"`
	Owner pricingOwnerRef    `yaml:"owner"`
}

// pricingOwnerRef is the owner reference in Pricing metadata.
type pricingOwnerRef struct {
	Kind string `yaml:"kind"`
	ID   string `yaml:"id"`
}

// PricingDTO is the full Pricing YAML document emitted by this tool.
type PricingDTO struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   pricingMeta `yaml:"metadata"`
	Spec       pricingSpec `yaml:"spec"`
}

// TranslateResult holds the translated DTOs.
type TranslateResult struct {
	Providers       []*manifest.ProviderDTO
	Hosts           []*manifest.HostDTO
	Models          []*manifest.ModelDTO
	Pricings        []*PricingDTO
	SkippedMode     int
	SkippedProvider int
}

// Translate converts filtered LiteLLM entries into new-arch manifest DTOs.
func Translate(entries map[string]Entry, sourceVersion string) (*TranslateResult, error) {
	today := time.Now()
	if sourceVersion == "" {
		sourceVersion = today.Format("2006-01-02")
	}

	result := &TranslateResult{}
	seenProviders := map[string]bool{}

	chatEntries := map[string]Entry{}
	for k, e := range entries {
		if e.Mode != "chat" {
			result.SkippedMode++
			continue
		}
		if _, skip := skippedProviders[e.LiteLLMProvider]; skip {
			result.SkippedProvider++
			continue
		}
		if e.LiteLLMProvider == "" {
			result.SkippedProvider++
			continue
		}
		chatEntries[k] = e
	}

	groups := CollapseAliases(chatEntries)

	for _, g := range groups {
		e := g.Entry
		pm, ok := knownProviders[e.LiteLLMProvider]
		if !ok {
			pm = fallbackProvider(e.LiteLLMProvider)
		}

		if !seenProviders[e.LiteLLMProvider] {
			seenProviders[e.LiteLLMProvider] = true
			result.Providers = append(result.Providers, buildProvider(pm))
			result.Hosts = append(result.Hosts, buildHost(pm))
		}

		m, err := buildModel(g, pm, sourceVersion, today)
		if err != nil {
			return nil, fmt.Errorf("translate: model %q: %w", g.ModelName, err)
		}
		result.Models = append(result.Models, m)

		if p := buildPricing(g, pm); p != nil {
			result.Pricings = append(result.Pricings, p)
		}
	}

	sort.Slice(result.Providers, func(i, j int) bool { return result.Providers[i].Metadata.Name < result.Providers[j].Metadata.Name })
	sort.Slice(result.Hosts, func(i, j int) bool { return result.Hosts[i].Metadata.Name < result.Hosts[j].Metadata.Name })
	sort.Slice(result.Models, func(i, j int) bool { return result.Models[i].Metadata.Name < result.Models[j].Metadata.Name })
	sort.Slice(result.Pricings, func(i, j int) bool { return result.Pricings[i].Metadata.Name < result.Pricings[j].Metadata.Name })

	return result, nil
}

func buildProvider(pm providerMeta) *manifest.ProviderDTO {
	return &manifest.ProviderDTO{
		APIVersion: manifest.APIVersion,
		Kind:       "Provider",
		Metadata: manifest.WireMeta{
			Name:        pm.name,
			DisplayName: pm.displayName,
			Description: pm.description,
			Owner:       manifest.WireOwner{Kind: meta.OwnerSystem},
		},
		Spec: manifest.ProviderSpec{
			HomepageURL:   pm.homepageURL,
			DocsURL:       pm.docsURL,
			StatusPageURL: pm.statusPageURL,
			Icon:          iconFromPath(pm.iconPath),
		},
	}
}

func buildHost(pm providerMeta) *manifest.HostDTO {
	return &manifest.HostDTO{
		APIVersion: manifest.APIVersion,
		Kind:       "Host",
		Metadata: manifest.WireMeta{
			Name:        pm.name,
			DisplayName: pm.displayName,
			Owner:       manifest.WireOwner{Kind: meta.OwnerSystem},
		},
		Spec: manifest.HostSpec{
			BaseURL:       pm.baseURL,
			HomepageURL:   pm.homepageURL,
			DocsURL:       pm.docsURL,
			ConsoleURL:    pm.consoleURL,
			StatusPageURL: pm.statusPageURL,
			Icon:          iconFromPath(pm.iconPath),
		},
	}
}

func iconFromPath(p string) *meta.Icon {
	if p == "" {
		return nil
	}
	return &meta.Icon{Path: p}
}

func addRate(rates []pricingRate, meter string, usdPerToken float64, aboveTokens int) []pricingRate {
	if usdPerToken == 0 {
		return rates
	}
	return append(rates, pricingRate{
		Meter:       meter,
		Unit:        "per_million",
		Amount:      usdPerToken * 1_000_000,
		AboveTokens: aboveTokens,
	})
}

func buildPricing(g Group, pm providerMeta) *PricingDTO {
	e := g.Entry
	var rates []pricingRate

	rates = addRate(rates, "tokens.input", e.InputCostPerToken, 0)
	rates = addRate(rates, "tokens.output", e.OutputCostPerToken, 0)
	rates = addRate(rates, "tokens.cache_creation", e.CacheCreationInputTokenCost, 0)
	rates = addRate(rates, "tokens.cache_read", e.CacheReadInputTokenCost, 0)
	rates = addRate(rates, "tokens.reasoning", e.OutputCostPerReasoningToken, 0)

	rates = addRate(rates, "tokens.input", e.InputCostPerTokenAbove128kTokens, 128_000)
	rates = addRate(rates, "tokens.output", e.OutputCostPerTokenAbove128kTokens, 128_000)
	rates = addRate(rates, "tokens.input", e.InputCostPerTokenAbove200kTokens, 200_000)
	rates = addRate(rates, "tokens.output", e.OutputCostPerTokenAbove200kTokens, 200_000)
	rates = addRate(rates, "tokens.input", e.InputCostPerTokenAbove272kTokens, 272_000)
	rates = addRate(rates, "tokens.output", e.OutputCostPerTokenAbove272kTokens, 272_000)

	if len(rates) == 0 {
		return nil
	}

	name := pm.name + "-" + SanitizeFilename(g.ModelName)
	return &PricingDTO{
		APIVersion: manifest.APIVersion,
		Kind:       "Pricing",
		Metadata: pricingMeta{
			Name:  name,
			Owner: pricingOwnerRef{Kind: "host", ID: pm.name},
		},
		Spec: pricingSpec{
			Currency:     "USD",
			TargetModels: []string{g.ModelName},
			Rates:        rates,
		},
	}
}

func buildModel(g Group, pm providerMeta, version string, today time.Time) (*manifest.ModelDTO, error) {
	e := g.Entry

	inputMax := e.MaxInputTokens
	outputMax := e.MaxOutputTokens
	total := inputMax
	if inputMax == 0 && e.MaxTokens > 0 {
		inputMax = e.MaxTokens
		total = e.MaxTokens
	}
	if outputMax == 0 && e.MaxTokens > 0 {
		outputMax = e.MaxTokens
	}

	caps := model.Capabilities{
		Chat:              true,
		Streaming:         true,
		Tools:             e.SupportsFunctionCalling,
		ParallelTools:     e.SupportsParallelFunctionCalling,
		Vision:            e.SupportsVision,
		FileInput:         e.SupportsPDFInput,
		PromptCache:       e.SupportsPromptCaching,
		Reasoning:         e.SupportsReasoning,
		StructuredOutputs: e.SupportsResponseSchema || e.SupportsNativeStructuredOutput,
		SystemMessages:    e.SupportsSystemMessages,
		AssistantPrefill:  e.SupportsAssistantPrefill,
		AudioInput:        e.SupportsAudioInput,
		AudioOutput:       e.SupportsAudioOutput,
		WebSearch:         e.SupportsWebSearch,
		ComputerUse:       e.SupportsComputerUse,
		Batch:             hasBatchEndpoint(e),
	}

	family := inferFamily(g.ModelName)
	labels := map[string]string{
		"source":         "litellm",
		"source_version": version,
	}
	if family != "" {
		labels["family"] = family
	}

	snaps := make([]model.Snapshot, 0, len(g.Snapshots))
	for _, s := range g.Snapshots {
		snaps = append(snaps, model.Snapshot{
			Name:         s.Name,
			OriginalName: s.OriginalName,
			ReleasedAt:   s.ReleasedAt,
		})
	}

	// UpstreamName at the binding level is the legacy single-snapshot
	// field. Routing (stage 4) reads originalName from the resolved
	// snapshot instead; we populate this to the pointer's originalName
	// so the existing per-binding required field stays satisfied.
	pointerOriginal := g.Pointer
	for _, s := range g.Snapshots {
		if s.Name == g.Pointer {
			pointerOriginal = s.OriginalName
			break
		}
	}

	return &manifest.ModelDTO{
		APIVersion: manifest.APIVersion,
		Kind:       "Model",
		Metadata: manifest.WireMeta{
			Name:   g.ModelName,
			Labels: labels,
			Owner:  manifest.WireOwner{Kind: meta.OwnerProvider, Name: pm.name},
		},
		Spec: manifest.ModelSpec{
			Hosts: []manifest.HostBindingDTO{{
				Host:         pm.name,
				UpstreamName: pointerOriginal,
				Adapter:      pm.adapter,
			}},
			Family:              family,
			Capabilities:        caps,
			Modalities:          inferModalities(e),
			ContextWindowInput:  inputMax,
			ContextWindowOutput: outputMax,
			ContextWindowTotal:  total,
			Deprecation:         deprecationInfo(e, today),
			Aliases:             g.Aliases,
			Snapshots:           snaps,
			Pointer:             g.Pointer,
		},
	}, nil
}
