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
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, fmt.Errorf("gemini parse_request: %w", err)
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
	}

	if len(wire.Tools) > 0 {
		tc := &v1.ToolsConfig{}
		for _, rawTool := range wire.Tools {
			var gt geminiTool
			if err := json.Unmarshal(rawTool, &gt); err != nil {
				return nil, fmt.Errorf("gemini parse_request: tool: %w", err)
			}
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
			if err := json.Unmarshal(wire.ToolConfig, &tcWire); err == nil && tcWire.FunctionCallingConfig != nil {
				tc.Choice = geminiToolModeToChoice(tcWire.FunctionCallingConfig)
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
		switch c.Role {
		case "user":
			// user may have text parts or functionResponse parts
			var textParts []v1.Part
			hasFunctionResponse := false
			for _, p := range c.Parts {
				if p.FunctionResponse != nil {
					hasFunctionResponse = true
					output := ""
					if len(p.FunctionResponse.Response) > 0 {
						output = string(p.FunctionResponse.Response)
					}
					items = append(items, &v1.FunctionCallOutput{
						CallID: p.FunctionResponse.Name,
						Output: output,
					})
				} else if p.Text != "" {
					textParts = append(textParts, &v1.TextPart{Text: p.Text})
				} else if p.InlineData != nil {
					url := "data:" + p.InlineData.MIMEType + ";base64," + p.InlineData.Data
					textParts = append(textParts, &v1.ImagePart{ImageURL: url})
				} else if p.FileData != nil {
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
			var textParts []v1.Part
			for _, p := range c.Parts {
				if p.FunctionCall != nil {
					args := "{}"
					if len(p.FunctionCall.Args) > 0 {
						args = string(p.FunctionCall.Args)
					}
					items = append(items, &v1.FunctionCall{
						CallID:    p.FunctionCall.Name,
						Name:      p.FunctionCall.Name,
						Arguments: args,
					})
				} else if p.Text != "" && p.Thought {
					items = append(items, &v1.Reasoning{Content: p.Text, Summary: []v1.SummaryText{{Text: p.Text}}})
				} else if p.Text != "" {
					textParts = append(textParts, &v1.OutputTextPart{Text: p.Text})
				}
			}
			if len(textParts) > 0 {
				items = append(items, &v1.Message{Role: v1.RoleAssistant, Content: textParts})
			}

		default:
			// Unknown roles: treat as user.
			var textParts []v1.Part
			for _, p := range c.Parts {
				if p.Text != "" {
					textParts = append(textParts, &v1.TextPart{Text: p.Text})
				}
			}
			if len(textParts) > 0 {
				items = append(items, &v1.Message{Role: v1.RoleUser, Content: textParts})
			}
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
		return &v1.ToolChoice{Mode: "required"}
	case "NONE":
		return &v1.ToolChoice{Mode: "none"}
	default:
		return &v1.ToolChoice{Mode: "auto"}
	}
}
