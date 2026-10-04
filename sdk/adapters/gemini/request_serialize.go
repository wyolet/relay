package gemini

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- SerializeRequest ----

// SerializeRequest encodes a canonical *v1.Request to a Gemini generateContent body.
func (GeminiTranslator) SerializeRequest(req *v1.Request) ([]byte, error) {
	out := &geminiRequest{}

	// Resolve model opts: prefer entry keyed by req.Model[0], fall back to
	// single entry when ModelConfig has exactly one key.
	modelKey := ""
	if len(req.Model) > 0 {
		modelKey = req.Model[0]
	}
	opts := resolveModelOpts(req.ModelConfig, modelKey)

	// canonical: User dropped — generateContent has no end-user identifier.
	// canonical: Metadata dropped — generateContent takes no request metadata
	// (labels exist only on Vertex, a different endpoint).
	// canonical: Extensions dropped — no Gemini-owned extension keys exist;
	// rule 7 has an adapter ignore keys it does not own.
	// canonical: CacheConfig dropped — Gemini caches implicitly; it has no
	// breakpoint, key, or retention knob on generateContent.

	// systemInstruction from Instructions.
	sysText := req.Instructions
	if sysText != "" {
		out.SystemInstruction = &geminiContent{
			Parts: []geminiPart{{Text: sysText}},
		}
	}

	// generationConfig + tools from opts.
	if opts != nil {
		gc := &generationConfig{}
		hasGC := false
		if s := opts.Sampling; s != nil {
			gc.Temperature = s.Temperature
			gc.TopP = s.TopP
			gc.TopK = s.TopK
			gc.MaxOutputTokens = s.MaxTokens
			gc.StopSequences = s.Stop
			gc.Seed = s.Seed
			gc.FrequencyPenalty = s.FrequencyPenalty
			gc.PresencePenalty = s.PresencePenalty
			hasGC = true
		}
		if r := opts.Reasoning; r != nil {
			tc := &thinkingConfig{IncludeThoughts: true}
			if r.BudgetTokens != nil {
				tc.ThinkingBudget = *r.BudgetTokens
			}
			// canonical: Reasoning.Effort dropped — thinkingLevel is rejected by
			// pre-3 models and canonical carries no model generation to gate it;
			// includeThoughts plus the budget carry the reasoning intent.
			// canonical: Reasoning.Summary dropped — Gemini only toggles
			// includeThoughts (already on), with no summary granularity.
			gc.ThinkingConfig = tc
			hasGC = true
		}
		// canonical: Output.Verbosity dropped — generateContent has no
		// verbosity control.
		// canonical: Format.Name/Description/Strict dropped — responseSchema
		// takes the bare schema only.
		if o := opts.Output; o != nil && o.Format != nil {
			switch o.Format.Type {
			case "json_object":
				gc.ResponseMIMEType = "application/json"
				hasGC = true
			case "json_schema":
				gc.ResponseMIMEType = "application/json"
				if len(o.Format.Schema) > 0 {
					gc.ResponseSchema = o.Format.Schema
				}
				hasGC = true
			}
		}
		if hasGC {
			out.GenerationConfig = gc
		}
	}

	// Tools are task-level (req.Tools), shared across models — not per-model.
	if tc := req.Tools; tc != nil {
		var decls []functionDeclaration
		for _, tool := range tc.Definitions {
			ft, ok := tool.(*v1.FunctionTool)
			if !ok {
				return nil, fmt.Errorf("gemini serialize_request: unsupported tool type %T", tool)
			}
			schema := ft.Parameters
			if schema == nil {
				schema = json.RawMessage(`{}`)
			}
			// canonical: FunctionTool.Strict dropped — function declarations
			// take no strict-schema flag.
			// canonical: FunctionTool.ProviderData dropped — it holds another
			// vendor's original tool definition; Gemini gets the lowered schema.
			decls = append(decls, functionDeclaration{
				Name:        ft.Name,
				Description: ft.Description,
				Parameters:  schema,
			})
		}
		if len(decls) > 0 {
			out.Tools = []geminiTool{{FunctionDeclarations: decls}}
		}
		if tc.Choice != nil {
			out.ToolConfig = canonicalChoiceToGemini(tc.Choice)
		}
		// canonical: Tools.Parallel dropped — Gemini has no switch for
		// parallel function calls.
	}

	// Build contents from canonical Input. LEADING system/developer items and
	// hoist-flagged items merge into systemInstruction; other positional ones
	// stay in contents (marker-wrapped user turns) so the prefix stays
	// cache-stable.
	input, hoistedSys := v1.SplitHoistedSystem(req.Input)
	contents, extraSys, err := canonicalItemsToGemini(input)
	if err != nil {
		return nil, fmt.Errorf("gemini serialize_request: %w", err)
	}
	out.Contents = contents
	for _, extra := range []string{extraSys, hoistedSys} {
		if extra == "" {
			continue
		}
		if out.SystemInstruction != nil {
			out.SystemInstruction.Parts = append(out.SystemInstruction.Parts, geminiPart{Text: extra})
		} else {
			out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: extra}}}
		}
	}

	return json.Marshal(out)
}

// canonicalChoiceToGemini maps canonical ToolChoice to Gemini toolConfig.
func canonicalChoiceToGemini(tc *v1.ToolChoice) *toolConfig {
	if tc == nil {
		return nil
	}
	cfg := &functionCallingConfig{}
	switch tc.Mode {
	case "auto":
		cfg.Mode = "AUTO"
	case "required":
		cfg.Mode = "ANY"
	case "none":
		cfg.Mode = "NONE"
	case "function":
		cfg.Mode = "ANY"
		if tc.FunctionName != "" {
			cfg.AllowedFunctionNames = []string{tc.FunctionName}
		}
	default:
		cfg.Mode = "AUTO"
	}
	return &toolConfig{FunctionCallingConfig: cfg}
}
