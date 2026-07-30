package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

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
			gc.ThinkingConfig = tc
			hasGC = true
		}
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

// canonicalItemsToGemini converts canonical []v1.Item to Gemini contents.
// System/developer text from LEADING items is returned separately for
// systemInstruction; positional system items stay in contents as
// marker-wrapped user turns.
func canonicalItemsToGemini(items []v1.Item) ([]geminiContent, string, error) {
	var contents []geminiContent
	var sysExtra []string

	// Gemini requires strict user/model alternation. We flush pending
	// function calls and results into the appropriate role content.
	var pendingFCs []v1.FunctionCall
	var pendingFCOs []v1.FunctionCallOutput

	// Positional system turns deferred past an open tool round; drained
	// right after the functionResponse user turn.
	var pendingSystem []geminiContent
	// seenConversation flips once any non-system item lands in contents;
	// system/developer items before that merge into systemInstruction.
	seenConversation := false

	flushFCs := func() {
		if len(pendingFCs) == 0 {
			return
		}
		var parts []geminiPart
		for _, fc := range pendingFCs {
			var argsObj json.RawMessage
			if fc.Arguments != "" {
				argsObj = json.RawMessage(fc.Arguments)
			} else {
				argsObj = json.RawMessage(`{}`)
			}
			p := geminiPart{FunctionCall: &geminiFC{Name: fc.Name, Args: argsObj}}
			if sig := thoughtSignatureFrom(fc.ProviderData); sig != "" {
				p.ThoughtSignature = sig
			}
			parts = append(parts, p)
		}
		contents = append(contents, geminiContent{Role: "model", Parts: parts})
		pendingFCs = pendingFCs[:0]
	}

	flushFCOs := func() {
		if len(pendingFCOs) == 0 {
			return
		}
		var parts []geminiPart
		for _, fco := range pendingFCOs {
			resp := fco.Output
			if resp == "" && len(fco.Content) > 0 {
				var sb strings.Builder
				for _, p := range fco.Content {
					if tp, ok := p.(*v1.TextPart); ok {
						sb.WriteString(tp.Text)
					}
				}
				resp = sb.String()
			}
			// Gemini functionResponse.response must be a JSON object.
			trimmedResp := []byte(strings.TrimSpace(resp))
			var respRaw json.RawMessage
			if len(trimmedResp) > 0 && trimmedResp[0] == '{' && json.Unmarshal(trimmedResp, &respRaw) == nil {
				// Already a JSON object.
			} else {
				var raw json.RawMessage
				if json.Unmarshal(trimmedResp, &raw) == nil {
					b, _ := json.Marshal(map[string]json.RawMessage{"output": raw})
					respRaw = b
				} else {
					b, _ := json.Marshal(map[string]string{"output": resp})
					respRaw = b
				}
			}
			parts = append(parts, geminiPart{FunctionResponse: &geminiFR{
				Name:     geminiFuncNameFromCallID(fco.CallID),
				Response: respRaw,
			}})
		}
		contents = append(contents, geminiContent{Role: "user", Parts: parts})
		pendingFCOs = pendingFCOs[:0]
		contents = append(contents, pendingSystem...)
		pendingSystem = pendingSystem[:0]
	}

	for _, item := range items {
		switch v := item.(type) {
		case *v1.Message:
			if v.Role == v1.RoleSystem || v.Role == v1.RoleDeveloper {
				var sb strings.Builder
				for _, p := range v.Content {
					switch tp := p.(type) {
					case *v1.TextPart:
						sb.WriteString(tp.Text)
					case *v1.OutputTextPart:
						sb.WriteString(tp.Text)
					}
				}
				s := sb.String()
				if s == "" {
					continue
				}
				// Leading system/developer items merge into systemInstruction;
				// positional ones stay in place as marker-wrapped user turns
				// (the wire has no mid-conversation system concept) so the
				// instruction keeps its position and the prefix stays stable
				// for context caching. Round-trips via v1.UnwrapSystemMarker.
				if !seenConversation {
					sysExtra = append(sysExtra, s)
					continue
				}
				sysTurn := geminiContent{Role: "user", Parts: []geminiPart{{Text: v1.WrapSystemMarker(s)}}}
				// Never split a functionCall from its functionResponse: an open
				// tool round defers the turn to just after the responses.
				if len(pendingFCs) > 0 || len(pendingFCOs) > 0 {
					pendingSystem = append(pendingSystem, sysTurn)
					continue
				}
				contents = append(contents, sysTurn)
				continue
			}

			seenConversation = true
			flushFCs()
			flushFCOs()

			switch v.Role {
			case v1.RoleUser:
				parts, err := canonicalPartsToGemini(v.Content)
				if err != nil {
					return nil, "", err
				}
				contents = append(contents, geminiContent{Role: "user", Parts: parts})

			case v1.RoleAssistant:
				parts, err := canonicalPartsToGemini(v.Content)
				if err != nil {
					return nil, "", err
				}
				contents = append(contents, geminiContent{Role: "model", Parts: parts})
			}

		case *v1.FunctionCall:
			seenConversation = true
			flushFCOs()
			pendingFCs = append(pendingFCs, *v)

		case *v1.FunctionCallOutput:
			seenConversation = true
			flushFCs()
			pendingFCOs = append(pendingFCOs, *v)

		case *v1.Reasoning:
			// Emit as a thought part so that thoughtSignature is round-tripped.
			// If there's no ProviderData (no signature), Gemini ignores unknown
			// thought parts in history, so this is safe to always emit.
			flushFCOs()
			text := v.Content
			if text == "" && len(v.Summary) > 0 {
				text = v.Summary[0].Text
			}
			if text != "" {
				seenConversation = true
				p := geminiPart{Text: text, Thought: true}
				if sig := thoughtSignatureFrom(v.ProviderData); sig != "" {
					p.ThoughtSignature = sig
				}
				contents = append(contents, geminiContent{Role: "model", Parts: []geminiPart{p}})
			}
		}
	}

	flushFCs()
	flushFCOs()
	contents = append(contents, pendingSystem...)

	return contents, strings.Join(sysExtra, "\n"), nil
}

func canonicalPartsToGemini(parts []v1.Part) ([]geminiPart, error) {
	var out []geminiPart
	for _, p := range parts {
		switch v := p.(type) {
		case *v1.TextPart:
			out = append(out, geminiPart{Text: v.Text})
		case *v1.OutputTextPart:
			out = append(out, geminiPart{Text: v.Text})
		case *v1.ImagePart:
			gp, err := canonicalImageToGemini(v.ImageURL)
			if err != nil {
				return nil, err
			}
			out = append(out, gp)
		case *v1.FilePart:
			if v.FileData != "" {
				mt := v.MediaType
				if mt == "" {
					mt = "application/octet-stream"
				}
				out = append(out, geminiPart{InlineData: &inlineData{MIMEType: mt, Data: v.FileData}})
			} else if v.FileURL != "" {
				out = append(out, geminiPart{FileData: &fileData{FileURI: v.FileURL, MIMEType: v.MediaType}})
			} else {
				return nil, fmt.Errorf("gemini serialize_request: file part has no data or URL")
			}
		default:
			return nil, fmt.Errorf("gemini serialize_request: unsupported part type %T", p)
		}
	}
	return out, nil
}

func canonicalImageToGemini(url string) (geminiPart, error) {
	if strings.HasPrefix(url, "data:") {
		rest := url[5:]
		semi := strings.Index(rest, ";")
		comma := strings.Index(rest, ",")
		if semi >= 0 && comma > semi {
			mt := rest[:semi]
			data := rest[comma+1:]
			return geminiPart{InlineData: &inlineData{MIMEType: mt, Data: data}}, nil
		}
	}
	// Plain URL — use fileData.
	return geminiPart{FileData: &fileData{FileURI: url}}, nil
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
