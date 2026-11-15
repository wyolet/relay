package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ---- ParseRequest ----

// ParseRequest decodes a Gemini generateContent request body into canonical *v1.Request.
func (GeminiTranslator) ParseRequest(body []byte) (*v1.Request, error) {
	var wire struct {
		Contents          []json.RawMessage `json:"contents"`
		SystemInstruction json.RawMessage   `json:"systemInstruction"`
		GenerationConfig  json.RawMessage   `json:"generationConfig"`
		Tools             []json.RawMessage `json:"tools"`
		ToolConfig        json.RawMessage   `json:"toolConfig"`
		// Model is not part of the Gemini body (it lives in the URL), but we
		// accept it as a convenience field so tests can round-trip cleanly.
		Model string `json:"model"`

		SystemInstructionSnake json.RawMessage `json:"system_instruction"`
		GenerationConfigSnake  json.RawMessage `json:"generation_config"`
		ToolConfigSnake        json.RawMessage `json:"tool_config"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("gemini parse_request: %w", err)
	}
	if wire.SystemInstruction == nil {
		wire.SystemInstruction = wire.SystemInstructionSnake
	}
	if wire.GenerationConfig == nil {
		wire.GenerationConfig = wire.GenerationConfigSnake
	}
	if wire.ToolConfig == nil {
		wire.ToolConfig = wire.ToolConfigSnake
	}

	req := &v1.Request{}
	if wire.Model != "" {
		req.Model = v1.ModelRefs{wire.Model}
	}

	// systemInstruction → Instructions
	if len(wire.SystemInstruction) > 0 && string(wire.SystemInstruction) != "null" {
		req.Instructions = geminiExtractSystemText(wire.SystemInstruction)
	}

	// Build model opts from generationConfig + tools.
	opts := &v1.ModelOpts{}
	hasOpts := false

	if len(wire.GenerationConfig) > 0 && string(wire.GenerationConfig) != "null" {
		var gc generationConfig
		if err := json.Unmarshal(wire.GenerationConfig, &gc); err != nil {
			return nil, fmt.Errorf("gemini parse_request: generationConfig: %w", err)
		}
		gc.fillFromSnakeCase()
		sp := &v1.SamplingParams{}
		hasSampling := false
		if gc.Temperature != nil {
			sp.Temperature = gc.Temperature
			hasSampling = true
		}
		if gc.TopP != nil {
			sp.TopP = gc.TopP
			hasSampling = true
		}
		if gc.TopK != nil {
			sp.TopK = gc.TopK
			hasSampling = true
		}
		if gc.MaxOutputTokens != nil {
			sp.MaxTokens = gc.MaxOutputTokens
			hasSampling = true
		}
		if len(gc.StopSequences) > 0 {
			sp.Stop = gc.StopSequences
			hasSampling = true
		}
		if gc.Seed != nil {
			sp.Seed = gc.Seed
			hasSampling = true
		}
		if gc.FrequencyPenalty != nil {
			sp.FrequencyPenalty = gc.FrequencyPenalty
			hasSampling = true
		}
		if gc.PresencePenalty != nil {
			sp.PresencePenalty = gc.PresencePenalty
			hasSampling = true
		}
		if hasSampling {
			opts.Sampling = sp
			hasOpts = true
		}
		if gc.ThinkingConfig != nil && gc.ThinkingConfig.ThinkingBudget > 0 {
			rc := &v1.ReasoningConfig{BudgetTokens: &gc.ThinkingConfig.ThinkingBudget}
			opts.Reasoning = rc
			hasOpts = true
		}
		if f := geminiResponseFormat(gc.ResponseMIMEType, gc.ResponseSchema); f != nil {
			opts.Output = &v1.OutputConfig{Format: f}
			hasOpts = true
		}
	}

	if len(wire.Tools) > 0 {
		tc := &v1.ToolsConfig{}
		for _, rawTool := range wire.Tools {
			var gt geminiTool
			if err := json.Unmarshal(rawTool, &gt); err != nil {
				return nil, fmt.Errorf("gemini parse_request: tool: %w", err)
			}
			gt.fillFromSnakeCase()
			for _, fd := range gt.FunctionDeclarations {
				schema := fd.Parameters
				if schema == nil {
					schema = json.RawMessage(`{}`)
				}
				tc.Definitions = append(tc.Definitions, &v1.FunctionTool{
					Name:        fd.Name,
					Description: fd.Description,
					Parameters:  schema,
				})
			}
		}
		if len(wire.ToolConfig) > 0 && string(wire.ToolConfig) != "null" {
			var tcWire toolConfig
			if err := json.Unmarshal(wire.ToolConfig, &tcWire); err == nil {
				tcWire.fillFromSnakeCase()
				if tcWire.FunctionCallingConfig != nil {
					tc.Choice = geminiToolModeToChoice(tcWire.FunctionCallingConfig)
				}
			}
		}
		req.Tools = tc
	}

	if hasOpts && len(req.Model) > 0 {
		req.ModelConfig = map[string]*v1.ModelOpts{req.Model[0]: opts}
	} else if hasOpts {
		req.ModelConfig = map[string]*v1.ModelOpts{"*": opts}
	}

	// Build Input from contents.
	items, err := geminiContentsToCanonical(wire.Contents)
	if err != nil {
		return nil, fmt.Errorf("gemini parse_request: contents: %w", err)
	}
	req.Input = items

	return req, nil
}

// geminiResponseFormat inverts the serializer's json_object / json_schema mapping.
func geminiResponseFormat(mimeType string, schema json.RawMessage) *v1.Format {
	if mimeType != "application/json" {
		// canonical: responseMimeType other than application/json dropped — text/plain is the default and canonical Format has no enum (text/x.enum) type.
		return nil
	}
	if len(schema) > 0 && string(schema) != "null" {
		return &v1.Format{Type: "json_schema", Schema: schema}
	}
	return &v1.Format{Type: "json_object"}
}

// geminiFunctionOutput inverts the serializer's {"output": …} wrap of a non-object result. A wrap the serializer would not reproduce (an object value, or text that parses as JSON) stays whole so the round trip is stable.
func geminiFunctionOutput(resp json.RawMessage) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(resp, &obj) != nil || len(obj) != 1 {
		return string(resp)
	}
	inner, ok := obj["output"]
	if !ok || len(inner) == 0 || inner[0] == '{' {
		return string(resp)
	}
	if inner[0] != '"' {
		return string(inner)
	}
	var s string
	if json.Unmarshal(inner, &s) != nil || json.Valid([]byte(strings.TrimSpace(s))) {
		return string(resp)
	}
	return s
}

// geminiInlineDataPart picks the canonical part from the mime type: an image rides an ImagePart data URL, anything else a FilePart.
func geminiInlineDataPart(d *inlineData) v1.Part {
	if strings.HasPrefix(d.MIMEType, "image/") {
		return &v1.ImagePart{ImageURL: "data:" + d.MIMEType + ";base64," + d.Data}
	}
	return &v1.FilePart{FileData: d.Data, MediaType: d.MIMEType}
}

// geminiExtractSystemText reads systemInstruction content parts as plain text.
func geminiExtractSystemText(raw json.RawMessage) string {
	var c geminiContent
	if err := json.Unmarshal(raw, &c); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// geminiContentsToCanonical converts Gemini contents array to canonical []v1.Item.
func geminiContentsToCanonical(raws []json.RawMessage) ([]v1.Item, error) {
	var items []v1.Item
	for _, raw := range raws {
		var c geminiContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, err
		}
		for i := range c.Parts {
			c.Parts[i].fillFromSnakeCase()
		}
		switch c.Role {
		default:
			// Any role but model reads as user, as the API reads a content without one.
			var textParts []v1.Part
			hasFunctionResponse := false
			for _, p := range c.Parts {
				if p.FunctionResponse != nil {
					hasFunctionResponse = true
					output := ""
					if len(p.FunctionResponse.Response) > 0 {
						output = geminiFunctionOutput(p.FunctionResponse.Response)
					}
					callID := p.FunctionResponse.ID
					if callID == "" {
						callID = p.FunctionResponse.Name
					}
					items = append(items, &v1.FunctionCallOutput{
						CallID: callID,
						Output: output,
					})
				} else if p.Text != "" {
					textParts = append(textParts, &v1.TextPart{Text: p.Text})
				} else if p.InlineData != nil {
					textParts = append(textParts, geminiInlineDataPart(p.InlineData))
				} else if p.FileData != nil {
					// Stays a FilePart even for an image: ImagePart has no media type for a URL, so the mimeType would be lost.
					textParts = append(textParts, &v1.FilePart{
						FileURL:   p.FileData.FileURI,
						MediaType: p.FileData.MIMEType,
					})
				}
			}
			if len(textParts) == 1 && !hasFunctionResponse {
				// A whole-turn marker wrap is a positional system item that had
				// to ride as user text (no system role on this wire) — restore
				// the role so the marker never surfaces as user text on replay.
				if tp, ok := textParts[0].(*v1.TextPart); ok {
					if inner, ok := v1.UnwrapSystemMarker(tp.Text); ok {
						items = append(items, &v1.Message{Role: v1.RoleSystem, Content: []v1.Part{&v1.TextPart{Text: inner}}})
						continue
					}
				}
			}
			if len(textParts) > 0 {
				items = append(items, &v1.Message{Role: v1.RoleUser, Content: textParts})
			}

		case "model":
			turnStart := len(items)
			var textParts []v1.Part
			var textSig json.RawMessage
			// Text is flushed before each call or thought so items keep wire order, and after a signed part so the signature stays on it.
			flushText := func() {
				if len(textParts) > 0 {
					items = append(items, &v1.Message{Role: v1.RoleAssistant, Content: textParts, ProviderData: textSig})
					textParts, textSig = nil, nil
				}
			}
			for _, p := range c.Parts {
				if p.FunctionCall != nil {
					flushText()
					args := "{}"
					if len(p.FunctionCall.Args) > 0 {
						args = string(p.FunctionCall.Args)
					}
					callID := p.FunctionCall.ID
					if callID == "" {
						callID = p.FunctionCall.Name
					}
					fc := &v1.FunctionCall{
						CallID:    callID,
						Name:      p.FunctionCall.Name,
						Arguments: args,
					}
					if p.ThoughtSignature != "" {
						fc.ProviderData = thoughtSignatureJSON(p.ThoughtSignature)
					}
					items = append(items, fc)
				} else if p.Thought && (p.Text != "" || p.ThoughtSignature != "") {
					flushText()
					r := &v1.Reasoning{Content: p.Text}
					if p.Text != "" {
						r.Summary = []v1.SummaryText{{Text: p.Text}}
					}
					if p.ThoughtSignature != "" {
						r.ProviderData = thoughtSignatureJSON(p.ThoughtSignature)
					}
					items = append(items, r)
				} else if !p.Thought && p.Text != "" {
					textParts = append(textParts, &v1.OutputTextPart{Text: p.Text})
					if p.ThoughtSignature != "" {
						textSig = thoughtSignatureJSON(p.ThoughtSignature)
						flushText()
					}
				} else if !p.Thought && p.ThoughtSignature != "" && p.InlineData == nil && p.FileData == nil {
					if len(textParts) > 0 {
						textSig = emptyPartSignatureJSON(p.ThoughtSignature)
						flushText()
					} else if !signLastItem(items[turnStart:], p.ThoughtSignature) {
						items = append(items, &v1.Reasoning{ProviderData: emptyPartSignatureJSON(p.ThoughtSignature)})
					}
				}
			}
			flushText()
		}
	}
	return items, nil
}

// geminiToolModeToChoice maps Gemini functionCallingConfig to canonical ToolChoice.
func geminiToolModeToChoice(cfg *functionCallingConfig) *v1.ToolChoice {
	if cfg == nil {
		return nil
	}
	switch cfg.Mode {
	case "AUTO":
		return &v1.ToolChoice{Mode: "auto"}
	case "ANY":
		if len(cfg.AllowedFunctionNames) == 1 {
			return &v1.ToolChoice{Mode: "function", FunctionName: cfg.AllowedFunctionNames[0]}
		}
		return &v1.ToolChoice{Mode: "required", Allowed: cfg.AllowedFunctionNames}
	case "NONE":
		return &v1.ToolChoice{Mode: "none"}
	case "VALIDATED":
		// canonical: VALIDATED's constrained decoding dropped — canonical has no per-choice decoding switch.
		return &v1.ToolChoice{Mode: "auto", Allowed: cfg.AllowedFunctionNames}
	default:
		return &v1.ToolChoice{Mode: "auto"}
	}
}
