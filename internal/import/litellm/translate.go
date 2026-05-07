package litellm

import (
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/wyolet/relay/internal/catalog"
)

// providerMeta holds static display metadata for known litellm_provider values.
type providerMeta struct {
	kind          catalog.ProviderKind
	name          string // catalog name (metadata.name)
	displayName   string
	description   string
	baseURL       string
	homepageURL   string
	docsURL       string
	consoleURL    string
	statusPageURL string
	logoURL       string
}

// knownProviders is the static map from litellm_provider value to Wyolet metadata.
// Providers not in this map are skipped with a warning.
var knownProviders = map[string]providerMeta{
	"anthropic": {
		kind:          catalog.PKAnthropic,
		name:          "anthropic",
		displayName:   "Anthropic",
		description:   "Claude models from Anthropic.",
		baseURL:       "https://api.anthropic.com",
		homepageURL:   "https://www.anthropic.com",
		docsURL:       "https://docs.anthropic.com",
		consoleURL:    "https://console.anthropic.com",
		statusPageURL: "https://status.anthropic.com",
		logoURL:       "https://wyolet.dev/logos/anthropic.svg",
	},
	"openai": {
		kind:          catalog.PKOpenAI,
		name:          "openai",
		displayName:   "OpenAI",
		description:   "GPT models from OpenAI.",
		baseURL:       "https://api.openai.com",
		homepageURL:   "https://openai.com",
		docsURL:       "https://platform.openai.com/docs",
		consoleURL:    "https://platform.openai.com",
		statusPageURL: "https://status.openai.com",
		logoURL:       "https://wyolet.dev/logos/openai.svg",
	},
	"ollama": {
		kind:          catalog.PKOllama,
		name:          "ollama",
		displayName:   "Ollama",
		description:   "Run large language models locally with Ollama.",
		baseURL:       "http://host.docker.internal:11434",
		homepageURL:   "https://ollama.com",
		logoURL:       "https://wyolet.dev/logos/ollama.svg",
	},
}

// skippedProviders are litellm_provider values we explicitly skip with a reason.
var skippedProviders = map[string]string{
	"openai_compatible":          "requires custom config, can't be auto-imported",
	"vertex_ai-language-models":  "no provider client yet (PKGoogle not implemented)",
	"gemini":                     "no provider client yet (PKGoogle not implemented)",
	"bedrock":                    "Bedrock-routed models require BYO config",
	"groq":                       "provider client not implemented",
	"deepseek":                   "provider client not implemented",
	"fireworks_ai":               "provider client not implemented",
	"together_ai":                "provider client not implemented",
	"perplexity":                 "provider client not implemented",
	"mistral":                    "provider client not implemented",
	"cohere_chat":                "provider client not implemented",
	"cohere":                     "provider client not implemented",
	"replicate":                  "provider client not implemented",
	"huggingface":                "provider client not implemented",
	"aleph_alpha":                "provider client not implemented",
	"nlp_cloud":                  "provider client not implemented",
	"sagemaker":                  "provider client not implemented",
	"azure":                      "requires custom config",
	"azure_ai":                   "requires custom config",
}

// dateSuffixRE matches a date suffix like -20241022 at the end of a model name.
var dateSuffixRE = regexp.MustCompile(`-(\d{8})$`)

// suffixesToStrip are non-date suffixes that indicate aliases.
var suffixesToStrip = []string{"-latest", "-preview", "-current"}

// baseName strips date and alias suffixes from a model name to get the "family" name
// used for grouping during alias collapsing.
func baseName(name string) string {
	n := name
	// Strip date suffix first.
	n = dateSuffixRE.ReplaceAllString(n, "")
	// Strip trailing alias suffixes.
	for _, s := range suffixesToStrip {
		n = strings.TrimSuffix(n, s)
	}
	return n
}

// extractDateSuffix returns the date portion (YYYYMMDD) if present, else "".
func extractDateSuffix(name string) string {
	m := dateSuffixRE.FindStringSubmatch(name)
	if m != nil {
		return m[1]
	}
	return ""
}

// isAliasSuffix returns true if the name has a non-date alias suffix.
func isAliasSuffix(name string) bool {
	for _, s := range suffixesToStrip {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// Group holds the canonical entry and alias names for a model family.
type Group struct {
	Canonical string
	Aliases   []string
	Entry     Entry
}

// CollapseAliases groups entries by base name and picks a canonical per group.
// Groups with conflicting pricing/capabilities are NOT collapsed (returned as separate entries).
// Returns:
//   - collapsed []Group: groups with a single canonical and zero or more aliases
//   - conflicts []string: names that were not collapsed due to spec conflicts
func CollapseAliases(entries map[string]Entry) ([]Group, []string) {
	// Group by base name.
	byBase := map[string][]string{}
	for name := range entries {
		b := baseName(name)
		byBase[b] = append(byBase[b], name)
	}

	var groups []Group
	var conflicts []string

	for _, names := range byBase {
		if len(names) == 1 {
			groups = append(groups, Group{
				Canonical: names[0],
				Entry:     entries[names[0]],
			})
			continue
		}

		// Sort so order is deterministic.
		sort.Strings(names)

		// Pick canonical: prefer dated version (most specific).
		canonical := ""
		for _, n := range names {
			if extractDateSuffix(n) != "" {
				canonical = n
				break
			}
		}
		// If no dated version, pick the bare base name if present, else first alphabetically.
		if canonical == "" {
			b := baseName(names[0])
			for _, n := range names {
				if n == b {
					canonical = n
					break
				}
			}
		}
		if canonical == "" {
			canonical = names[0]
		}

		// Check for spec conflicts — compare pricing and capabilities against canonical.
		canonicalEntry := entries[canonical]
		hasConflict := false
		for _, n := range names {
			if n == canonical {
				continue
			}
			if entriesConflict(canonicalEntry, entries[n]) {
				slog.Warn("import litellm: alias conflict — emitting separately",
					"canonical", canonical, "alias", n)
				hasConflict = true
				conflicts = append(conflicts, n)
			}
		}

		if hasConflict {
			// Emit all as separate groups — each becomes its own Model.
			for _, n := range names {
				groups = append(groups, Group{
					Canonical: n,
					Entry:     entries[n],
				})
			}
			continue
		}

		var aliases []string
		for _, n := range names {
			if n != canonical {
				aliases = append(aliases, n)
			}
		}
		groups = append(groups, Group{
			Canonical: canonical,
			Aliases:   aliases,
			Entry:     canonicalEntry,
		})
	}

	// Sort for determinism.
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Canonical < groups[j].Canonical
	})

	return groups, conflicts
}

// entriesConflict returns true if two entries have meaningfully different
// pricing (non-zero mismatch) or capability flags.
func entriesConflict(a, b Entry) bool {
	// Compare key pricing fields; treat zero as "not set" (compatible).
	pricingFields := [][2]float64{
		{a.InputCostPerToken, b.InputCostPerToken},
		{a.OutputCostPerToken, b.OutputCostPerToken},
	}
	for _, pair := range pricingFields {
		av, bv := pair[0], pair[1]
		if av != 0 && bv != 0 && av != bv {
			return true
		}
	}
	// Compare capabilities.
	if a.SupportsFunctionCalling != b.SupportsFunctionCalling ||
		a.SupportsVision != b.SupportsVision ||
		a.SupportsReasoning != b.SupportsReasoning ||
		a.MaxInputTokens != b.MaxInputTokens {
		return true
	}
	return false
}

// inferFamily returns a model family string from the model name.
func inferFamily(name string) string {
	switch {
	case strings.HasPrefix(name, "claude-3") || strings.HasPrefix(name, "claude-4"):
		return "claude"
	case strings.HasPrefix(name, "claude-"):
		return "claude"
	case strings.HasPrefix(name, "gpt-"):
		return "gpt"
	case strings.HasPrefix(name, "o1-") || strings.HasPrefix(name, "o3-") || strings.HasPrefix(name, "o4-"):
		return "o-series"
	case strings.HasPrefix(name, "gemini-"):
		return "gemini"
	case strings.HasPrefix(name, "mistral-"):
		return "mistral"
	case strings.HasPrefix(name, "deepseek-"):
		return "deepseek"
	case strings.HasPrefix(name, "llama-"):
		return "llama"
	default:
		// First dash-separated token.
		parts := strings.SplitN(name, "-", 2)
		return parts[0]
	}
}

// inferModalities derives input/output modality lists from capability flags.
func inferModalities(e Entry) catalog.Modalities {
	inputs := []string{"text"}
	if e.SupportsVision {
		inputs = append(inputs, "image")
	}
	if e.SupportsAudioInput {
		inputs = append(inputs, "audio")
	}
	if e.SupportsPDFInput {
		// Only add "file" if not already implied by vision.
		inputs = append(inputs, "file")
	}

	outputs := []string{"text"}
	if e.SupportsAudioOutput {
		outputs = append(outputs, "audio")
	}

	return catalog.Modalities{Input: inputs, Output: outputs}
}

// hasBatchEndpoint returns true if the entry's supported_endpoints includes a batch path.
func hasBatchEndpoint(e Entry) bool {
	for _, ep := range e.SupportedEndpoints {
		if strings.Contains(ep, "batch") || strings.Contains(ep, "batches") {
			return true
		}
	}
	return false
}

// pricingRates builds the Rates map from an Entry. Rates are in USD per million tokens.
func pricingRates(e Entry) map[string]float64 {
	const M = 1_000_000.0
	r := map[string]float64{}

	if e.InputCostPerToken > 0 {
		r["tokens.input"] = e.InputCostPerToken * M
	}
	if e.OutputCostPerToken > 0 {
		r["tokens.output"] = e.OutputCostPerToken * M
	}
	if e.CacheCreationInputTokenCost > 0 {
		r["tokens.cache_creation"] = e.CacheCreationInputTokenCost * M
	}
	if e.CacheReadInputTokenCost > 0 {
		r["tokens.cache_read"] = e.CacheReadInputTokenCost * M
	}
	if e.OutputCostPerReasoningToken > 0 {
		r["tokens.reasoning"] = e.OutputCostPerReasoningToken * M
	}
	if e.InputCostPerTokenBatches > 0 {
		r["tokens.batch_input"] = e.InputCostPerTokenBatches * M
	}
	if e.OutputCostPerTokenBatches > 0 {
		r["tokens.batch_output"] = e.OutputCostPerTokenBatches * M
	}
	if e.InputCostPerImage > 0 {
		// Per-image: kept as-is (per_unit, not per_million).
		r["images"] = e.InputCostPerImage
	}
	if e.InputCostPerAudioToken > 0 {
		r["tokens.audio_input"] = e.InputCostPerAudioToken * M
	}
	if e.OutputCostPerAudioToken > 0 {
		r["tokens.audio_output"] = e.OutputCostPerAudioToken * M
	}

	if len(r) == 0 {
		return nil
	}
	return r
}

// deprecationInfo derives a Deprecation struct from the entry's deprecation_date field.
// today is passed in so translation remains deterministic in tests (pass a fixed date).
func deprecationInfo(e Entry, today time.Time) *catalog.Deprecation {
	if e.DeprecationDate == "" {
		return nil
	}
	t, err := time.Parse("2006-01-02", e.DeprecationDate)
	if err != nil {
		return nil
	}
	if t.Before(today) {
		return &catalog.Deprecation{Status: "sunset", SunsetDate: e.DeprecationDate}
	}
	return &catalog.Deprecation{Status: "deprecated", SunsetDate: e.DeprecationDate}
}

// TranslateOptions controls translation behaviour.
type TranslateOptions struct {
	// SourceVersion is written into model labels as "source_version".
	SourceVersion string
	// Today is used for deprecation date comparison. Defaults to time.Now() if zero.
	Today time.Time
}

// TranslateResult is the output of a single translation pass over a filtered entry set.
type TranslateResult struct {
	// Providers contains one entry per unique provider seen (deduplicated by kind).
	Providers []*catalog.Provider
	// Models contains translated model entries, with aliases collapsed.
	Models []*catalog.Model
	// SkippedMode is the count of entries skipped because mode != "chat".
	SkippedMode int
	// SkippedProvider is the count of entries skipped due to unsupported provider.
	SkippedProvider int
}

// Translate translates a filtered map of LiteLLM entries into Wyolet catalog entities.
// The entries map must already be filtered (by --providers / --models flags) before calling.
// Alias collapsing is applied inside Translate.
func Translate(entries map[string]Entry, opts TranslateOptions) (*TranslateResult, error) {
	today := opts.Today
	if today.IsZero() {
		today = time.Now()
	}
	version := opts.SourceVersion
	if version == "" {
		version = today.Format("2006-01-02")
	}

	result := &TranslateResult{}
	seenProviders := map[string]bool{} // by litellm_provider value

	// Filter to chat-only, then collapse aliases.
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
		if _, ok := knownProviders[e.LiteLLMProvider]; !ok {
			// Unknown provider — skip with a log.
			slog.Info("import litellm: unknown provider — skipping",
				"provider", e.LiteLLMProvider, "model", k)
			result.SkippedProvider++
			continue
		}
		chatEntries[k] = e
	}

	groups, _ := CollapseAliases(chatEntries)

	for _, g := range groups {
		e := g.Entry
		meta, ok := knownProviders[e.LiteLLMProvider]
		if !ok {
			continue
		}

		// Emit provider once per litellm_provider value.
		if !seenProviders[e.LiteLLMProvider] {
			seenProviders[e.LiteLLMProvider] = true
			p := buildProvider(meta)
			result.Providers = append(result.Providers, p)
		}

		m, err := buildModel(g, meta, version, today)
		if err != nil {
			return nil, fmt.Errorf("translate: model %q: %w", g.Canonical, err)
		}
		result.Models = append(result.Models, m)
	}

	// Sort for determinism.
	sort.Slice(result.Providers, func(i, j int) bool {
		return result.Providers[i].Metadata.Name < result.Providers[j].Metadata.Name
	})
	sort.Slice(result.Models, func(i, j int) bool {
		return result.Models[i].Metadata.Name < result.Models[j].Metadata.Name
	})

	return result, nil
}

func buildProvider(meta providerMeta) *catalog.Provider {
	return &catalog.Provider{
		APIVersion: catalog.APIVersion,
		Kind:       catalog.KindProvider,
		Metadata: catalog.Metadata{
			Name: meta.name,
		},
		Spec: catalog.ProviderSpec{
			Kind:          meta.kind,
			BaseURL:       meta.baseURL,
			DisplayName:   meta.displayName,
			Description:   meta.description,
			HomepageURL:   meta.homepageURL,
			DocsURL:       meta.docsURL,
			ConsoleURL:    meta.consoleURL,
			StatusPageURL: meta.statusPageURL,
			LogoURL:       meta.logoURL,
		},
	}
}

func buildModel(g Group, meta providerMeta, version string, today time.Time) (*catalog.Model, error) {
	e := g.Entry

	// Context window.
	inputMax := e.MaxInputTokens
	outputMax := e.MaxOutputTokens
	total := inputMax
	if inputMax == 0 && e.MaxTokens > 0 {
		// max_tokens is legacy alias for max_output_tokens, but when max_input_tokens
		// is absent LiteLLM sometimes uses it as the total context.
		inputMax = e.MaxTokens
		total = e.MaxTokens
	}
	if outputMax == 0 && e.MaxTokens > 0 {
		outputMax = e.MaxTokens
	}

	// Capabilities.
	streaming := e.SupportsNativeStreaming
	if !streaming {
		// Default to true when the field is absent (zero value = false but field is usually omitted).
		// LiteLLM omits the field for many models that do stream.
		streaming = true
	}

	caps := catalog.Capabilities{
		Chat:             true,
		Streaming:        streaming,
		Tools:            e.SupportsFunctionCalling,
		ParallelTools:    e.SupportsParallelFunctionCalling,
		Vision:           e.SupportsVision,
		FileInput:        e.SupportsPDFInput,
		PromptCache:      e.SupportsPromptCaching,
		Reasoning:        e.SupportsReasoning,
		StructuredOutput: e.SupportsResponseSchema || e.SupportsNativeStructuredOutput,
		SystemMessages:   e.SupportsSystemMessages,
		AssistantPrefill: e.SupportsAssistantPrefill,
		AudioInput:       e.SupportsAudioInput,
		AudioOutput:      e.SupportsAudioOutput,
		WebSearch:        e.SupportsWebSearch,
		ComputerUse:      e.SupportsComputerUse,
		Batch:            hasBatchEndpoint(e),
	}

	modalities := inferModalities(e)
	rates := pricingRates(e)
	var pricing *catalog.Pricing
	if rates != nil {
		pricing = &catalog.Pricing{
			Currency: "USD",
			Unit:     catalog.PricingUnitPerMillion,
			Rates:    rates,
		}
	}

	dep := deprecationInfo(e, today)

	labels := map[string]string{
		"source":         "litellm",
		"source_version": version,
	}

	family := inferFamily(g.Canonical)
	if family != "" {
		labels["family"] = family
	}

	m := &catalog.Model{
		APIVersion: catalog.APIVersion,
		Kind:       catalog.KindModel,
		Metadata: catalog.Metadata{
			Name:   g.Canonical,
			Labels: labels,
		},
		Spec: catalog.ModelSpec{
			Provider:            meta.name,
			UpstreamName:        g.Canonical,
			Family:              family,
			Capabilities:        caps,
			Modalities:          modalities,
			Pricing:             pricing,
			ContextWindowInput:  inputMax,
			ContextWindowOutput: outputMax,
			ContextWindowTotal:  total,
			Deprecation:         dep,
			Aliases:             g.Aliases,
		},
	}

	return m, nil
}
