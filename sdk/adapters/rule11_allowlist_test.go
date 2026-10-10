package adapters_test

// Paths of fullRequest's elements, as flattenRequest names them.
const (
	userMsg   = "input[message:What is in these files?]"
	assistMsg = "input[message:Checking.]"
	devMsg    = "input[message:Answer in English.]"
	hoistMsg  = "input[message:Never name the tools.]"
	call1     = `input[function_call:lookup{"q":"cat"}]`
	call2     = `input[function_call:lookup{"q":"pdf"}]`
	result1   = "input[function_call_output:call_1]"
	result2   = "input[function_call_output:call_2]"
	reasoning = "input[reasoning]"
	tool      = "tools.definitions[function:lookup]"
	opts      = "model_config.*"
)

var (
	hoistedSystem = allowedDrop{
		paths:  []string{hoistMsg + ".*", "instructions"},
		folded: "a hoisted system item merges into instructions (v1.SplitHoistedSystem)",
	}
	developerAsSystem = allowedDrop{
		paths:  []string{devMsg + ".role"},
		folded: "developer and system items share the one marker-wrapped wire form and come back as system",
	}
	toolResultText = allowedDrop{
		paths:  []string{result2 + ".content[input_text].*"},
		folded: "text parts of a tool result flatten into its output string",
	}
)

func annotated(text string, paths ...string) allowedDrop {
	return allowedDrop{paths: paths, annotation: text}
}

func carriable(why string, paths ...string) allowedDrop {
	return allowedDrop{paths: paths, carriable: why}
}

// allowedDrops lists, per shape, every field fullRequest loses on
// canonical → wire → canonical. Entries match in order.
var allowedDrops = map[string][]allowedDrop{
	"openai-chat": {
		hoistedSystem,
		toolResultText,
		annotated("canonical: CacheConfig.Instructions/Tools dropped", "cache_config.instructions", "cache_config.tools"),
		annotated("canonical: ItemCacheConfig.Anchor dropped — OpenAI", userMsg+".cache_config.anchor"),
		annotated(`canonical: Extensions keys outside the "openai." prefix dropped`, "extensions.acme.trace"),
		annotated("canonical: FunctionCall.ID/Status/ProviderData dropped — a CC tool call", call1+".id", call1+".status", call1+".provider_data"),
		annotated("canonical: non-text tool-result parts (images, files) dropped", result2+".content[input_image].*"),
		annotated("canonical: OutputTextPart.Annotations dropped — CC", assistMsg+".content[output_text].annotations*"),
		annotated("canonical: Message.ID/Status/ProviderData dropped — CC", "input[message:*].id", "input[message:*].status", "input[message:*].provider_data"),
		annotated("canonical: FilePart.FileURL dropped — a CC", userMsg+".content[input_file].file_url"),
		annotated("canonical: FilePart.MediaType dropped — a CC", userMsg+".content[input_file*].media_type"),
		annotated("canonical: reasoning items dropped — CC input", reasoning+".*"),
		annotated("canonical: Reasoning.BudgetTokens dropped — Chat Completions", opts+".reasoning.budget_tokens"),
		annotated("canonical: Reasoning.Summary dropped — Chat Completions", opts+".reasoning.summary"),
		annotated("canonical: TopK dropped — Chat Completions", opts+".sampling.top_k"),
		annotated("canonical: FunctionTool.ProviderData dropped — it holds a", tool+".provider_data"),
	},
	"openai-responses": {
		hoistedSystem,
		annotated("canonical: CacheConfig.Instructions/Tools dropped", "cache_config.instructions", "cache_config.tools"),
		annotated("canonical: ItemCacheConfig.Anchor dropped — OpenAI", userMsg+".cache_config.anchor"),
		annotated("canonical: Extensions keys other than openai.service_tier dropped", "extensions.acme.trace", "extensions.openai.logit_bias", "extensions.openai.logprobs", "extensions.openai.store", "extensions.openai.top_logprobs"),
		annotated("canonical: service_tier dropped — it asks the upstream account", "extensions.openai.service_tier"),
		annotated("canonical: item Status dropped on Responses input", "input[*].status"),
		annotated("canonical: FunctionCall.ProviderData dropped unless it is the custom-call marker", call1+".provider_data"),
		annotated("canonical: Message.ProviderData dropped — Responses", userMsg+".provider_data"),
		annotated("canonical: text_citation annotations dropped", assistMsg+".content[output_text].annotations[text_citation].*"),
		annotated("canonical: FilePart.MediaType dropped — a Responses", userMsg+".content[input_file*].media_type"),
		carriable("a Responses reasoning item carries raw reasoning as content[].reasoning_text; responsesItemFromCanonical never writes Reasoning.Content", reasoning+".content"),
		annotated("canonical: BudgetTokens has no Responses wire equivalent", opts+".reasoning.budget_tokens"),
		annotated("canonical: Seed has no Responses wire equivalent", opts+".sampling.seed"),
		annotated("canonical: FrequencyPenalty has no Responses wire equivalent", opts+".sampling.frequency_penalty"),
		annotated("canonical: PresencePenalty has no Responses wire equivalent", opts+".sampling.presence_penalty"),
		annotated("canonical: stop_sequences dropped — the Responses API", opts+".sampling.stop"),
		annotated("canonical: TopK dropped — the Responses API", opts+".sampling.top_k"),
		annotated("canonical: FunctionTool.ProviderData dropped unless it holds a Responses custom tool", tool+".provider_data"),
	},
	"anthropic": {
		hoistedSystem,
		developerAsSystem,
		carriable("SerializeRequest emits cache_control breakpoints (system, last tool, anchored message, ttl) but ParseRequest never reads them back",
			"cache_config.instructions", "cache_config.tools", "cache_config.ttl", userMsg+".cache_config.anchor"),
		annotated("canonical: CacheConfig.Key dropped — Anthropic", "cache_config.key"),
		annotated("canonical: Extensions dropped — no Anthropic-owned", "extensions.*"),
		annotated("canonical: Metadata dropped — Anthropic metadata", "metadata.*"),
		annotated("canonical: FunctionCall.ID/Status/ProviderData dropped — a tool_use block", call1+".id", call1+".status", call1+".provider_data"),
		annotated("canonical: OutputTextPart.Annotations dropped — Anthropic", assistMsg+".content[output_text].annotations*"),
		annotated("canonical: Message.ID/Status/ProviderData dropped — Anthropic", "input[message:*].id", "input[message:*].status", "input[message:*].provider_data"),
		annotated("canonical: ImagePart.Detail dropped — Anthropic", userMsg+".content[input_image].detail"),
		carriable("SerializeRequest emits document blocks but ParseRequest skips them (anthropicBlockToPart), and Filename could ride the document title", userMsg+".content[input_file*"),
		annotated("canonical: Reasoning.ID/Status/Summary dropped — a thinking block", reasoning+".id", reasoning+".status", reasoning+".summary*"),
		annotated("canonical: Format.Name/Description/Strict dropped — the", opts+".output.format.name", opts+".output.format.description", opts+".output.format.strict"),
		annotated("canonical: Output.Format ignored when caller forces their own", opts+".output.format.*"),
		annotated("canonical: Output.Verbosity dropped — the Messages API", opts+".output.verbosity"),
		annotated("canonical: Reasoning.Summary dropped on the budget path", opts+".reasoning.summary"),
		annotated("canonical: Seed dropped — the Messages API", opts+".sampling.seed"),
		annotated("canonical: FrequencyPenalty dropped — the Messages API", opts+".sampling.frequency_penalty"),
		annotated("canonical: PresencePenalty dropped — the Messages API", opts+".sampling.presence_penalty"),
		annotated("canonical: Temperature/TopP/TopK dropped when reasoning is set", opts+".sampling.temperature", opts+".sampling.top_p", opts+".sampling.top_k"),
		annotated("canonical: FunctionTool.Strict dropped — Anthropic", tool+".strict"),
		annotated("canonical: FunctionTool.ProviderData dropped — it holds another", tool+".provider_data"),
		carriable("SerializeRequest writes disable_parallel_tool_use on tool_choice; anthropicParseToolChoice never maps it back to Tools.Parallel", "tools.parallel"),
	},
	"gemini": {
		hoistedSystem,
		developerAsSystem,
		toolResultText,
		{paths: []string{"model", "output_mode"}, folded: "the model and stream mode ride the generateContent URL, not the body"},
		annotated("canonical: CacheConfig dropped — Gemini", "cache_config.*"),
		annotated("canonical: ItemCacheConfig.Anchor dropped — Gemini", userMsg+".cache_config.anchor"),
		annotated("canonical: Extensions dropped — no Gemini-owned", "extensions.*"),
		annotated("canonical: User dropped — generateContent", "user"),
		annotated("canonical: Metadata dropped — generateContent", "metadata.*"),
		carriable("Gemini functionCall/functionResponse carry an id, but the adapter pairs by name: ParseRequest sets CallID to the function name, and SerializeRequest writes a foreign CallID into functionResponse.name",
			call1+".call_id", call2+".call_id"),
		annotated("canonical: FunctionCall.ID/Status dropped — a Gemini functionCall", call1+".id", call1+".status"),
		carriable("SerializeRequest writes thoughtSignature on functionCall and thought parts; ParseRequest never reads it back", call1+".provider_data", reasoning+".provider_data"),
		carriable(`SerializeRequest wraps a non-object tool result as {"output": …}; ParseRequest keeps the wrapper`, result1+".output"),
		annotated("canonical: non-text tool-result parts dropped — functionResponse.parts", result2+".content[input_image].*"),
		annotated("canonical: OutputTextPart.Annotations dropped — a Gemini", assistMsg+".content[output_text].annotations*"),
		annotated("canonical: Message.ID/Status/ProviderData dropped — Gemini", "input[message:*].id", "input[message:*].status", "input[message:*].provider_data"),
		annotated("canonical: ImagePart.Detail dropped — per-part mediaResolution", userMsg+".content[input_image].detail"),
		annotated("canonical: FilePart.Filename dropped — Gemini API", userMsg+".content[input_file*].filename"),
		carriable("ParseRequest types a part by wire field, not mime type (every inlineData becomes an image, every fileData a file), and SerializeRequest sends an image URL as fileData with no mimeType",
			userMsg+".content[*"),
		annotated("canonical: Reasoning.ID/Status dropped — a thought part", reasoning+".id", reasoning+".status"),
		annotated("canonical: Reasoning.Summary dropped when Content is set", reasoning+".summary*"),
		annotated("canonical: Format.Name/Description/Strict dropped — responseSchema", opts+".output.format.name", opts+".output.format.description", opts+".output.format.strict"),
		carriable("SerializeRequest writes responseMimeType/responseSchema; ParseRequest never maps them back to Output.Format", opts+".output.format.*"),
		annotated("canonical: Output.Verbosity dropped — generateContent", opts+".output.verbosity"),
		annotated("canonical: Reasoning.Effort dropped — thinkingLevel", opts+".reasoning.effort"),
		annotated("canonical: Reasoning.Summary dropped — Gemini only toggles", opts+".reasoning.summary"),
		annotated("canonical: FunctionTool.Strict dropped — function declarations", tool+".strict"),
		annotated("canonical: FunctionTool.ProviderData dropped — it holds another", tool+".provider_data"),
		annotated("canonical: Tools.Parallel dropped — Gemini", "tools.parallel"),
	},
}

// silentFeatureDrops are rejectedFeatures a shape serializes without an
// error while losing them; each is a skipped subtest until fixed.
var silentFeatureDrops = map[string]map[string]string{
	"openai-responses": {
		"server tool": "canonicalToResponsesRequest skips non-function tools where every other shape refuses them",
		"mcp tool":    "canonicalToResponsesRequest skips MCPTool although Responses has an mcp tool type (server_url, headers)",
	},
}
