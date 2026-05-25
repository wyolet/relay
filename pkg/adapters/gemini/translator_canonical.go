package gemini

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

// GeminiTranslator implements v1.Translator for the Gemini generateContent
// wire shape. Stateless value type; per-stream state lives in closures.
type GeminiTranslator struct{}

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
		opts.Tools = tc
		hasOpts = true
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
		if tc := opts.Tools; tc != nil {
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
	}

	// Build contents from canonical Input. System/developer role messages are
	// appended to systemInstruction rather than contents.
	contents, extraSys, err := canonicalItemsToGemini(req.Input)
	if err != nil {
		return nil, fmt.Errorf("gemini serialize_request: %w", err)
	}
	out.Contents = contents
	if extraSys != "" {
		if out.SystemInstruction != nil {
			out.SystemInstruction.Parts = append(out.SystemInstruction.Parts, geminiPart{Text: extraSys})
		} else {
			out.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: extraSys}}}
		}
	}

	return json.Marshal(out)
}

// ---- ParseResponse ----

// ParseResponse decodes a Gemini generateContent response body into canonical *v1.Response.
func (GeminiTranslator) ParseResponse(body []byte) (*v1.Response, error) {
	var gr geminiResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("gemini parse_response: %w", err)
	}

	resp := &v1.Response{
		ID:        fmt.Sprintf("gemini-%d", time.Now().UnixNano()),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     gr.ModelVersion,
	}

	if len(gr.Candidates) > 0 {
		cand := gr.Candidates[0]

		// Check for function calls in parts before mapping finish reason,
		// because the presence of a functionCall part overrides finishReason.
		hasFunctionCall := false
		if cand.Content != nil {
			for _, p := range cand.Content.Parts {
				if p.FunctionCall != nil {
					hasFunctionCall = true
					break
				}
			}
		}

		resp.Status, resp.FinishReason, resp.IncompleteDetails = geminiFinishReasonToCanonical(cand.FinishReason, hasFunctionCall)

		if cand.Content != nil {
			outputIndex := 0
			for _, p := range cand.Content.Parts {
				if p.FunctionCall != nil {
					args := "{}"
					if len(p.FunctionCall.Args) > 0 {
						args = string(p.FunctionCall.Args)
					}
					fc := &v1.FunctionCall{
						ID:        fmt.Sprintf("fc_%d", outputIndex),
						CallID:    geminiCallID(p.FunctionCall.Name, outputIndex), // Gemini has no call ID; synthesize a unique one
						Name:      p.FunctionCall.Name,
						Arguments: args,
						Status:    v1.StatusCompleted,
					}
					if p.ThoughtSignature != "" {
						fc.ProviderData = thoughtSignatureJSON(p.ThoughtSignature)
					}
					resp.Output = append(resp.Output, fc)
					outputIndex++
				} else if p.Text != "" && p.Thought {
					r := &v1.Reasoning{
						ID:      fmt.Sprintf("rs_%d", outputIndex),
						Content: p.Text,
						Summary: []v1.SummaryText{{Text: p.Text}},
						Status:  v1.StatusCompleted,
					}
					if p.ThoughtSignature != "" {
						r.ProviderData = thoughtSignatureJSON(p.ThoughtSignature)
					}
					resp.Output = append(resp.Output, r)
					outputIndex++
				} else if p.Text != "" {
					msg := &v1.Message{
						ID:      fmt.Sprintf("msg_%d", outputIndex),
						Status:  v1.StatusCompleted,
						Role:    v1.RoleAssistant,
						Content: []v1.Part{&v1.OutputTextPart{Text: p.Text}},
					}
					resp.Output = append(resp.Output, msg)
					outputIndex++
				}
			}
		}
	}

	resp.Usage = geminiUsageToTokens(gr.UsageMetadata)

	return resp, nil
}

// ---- SerializeResponse ----

// SerializeResponse encodes a canonical *v1.Response to a Gemini generateContent response body.
// req is unused — Gemini does not require request echo.
func (GeminiTranslator) SerializeResponse(resp *v1.Response, _ *v1.Request) ([]byte, error) {
	var parts []geminiPart
	finishReason := canonicalFinishReasonToGemini(resp.FinishReason, resp.IncompleteDetails)

	for _, item := range resp.Output {
		switch v := item.(type) {
		case *v1.Message:
			for _, p := range v.Content {
				switch tp := p.(type) {
				case *v1.OutputTextPart:
					parts = append(parts, geminiPart{Text: tp.Text})
				case *v1.TextPart:
					parts = append(parts, geminiPart{Text: tp.Text})
				}
			}
		case *v1.FunctionCall:
			var argsObj json.RawMessage
			if v.Arguments != "" {
				argsObj = json.RawMessage(v.Arguments)
			} else {
				argsObj = json.RawMessage(`{}`)
			}
			parts = append(parts, geminiPart{
				FunctionCall: &geminiFC{Name: v.Name, Args: argsObj},
			})
		case *v1.Reasoning:
			text := v.Content
			if text == "" && len(v.Summary) > 0 {
				text = v.Summary[0].Text
			}
			if text != "" {
				parts = append(parts, geminiPart{Text: text, Thought: true})
			}
		}
	}

	cand := map[string]any{
		"content":      map[string]any{"role": "model", "parts": parts},
		"finishReason": finishReason,
		"index":        0,
	}

	out := map[string]any{
		"candidates": []any{cand},
	}

	if len(resp.Usage) > 0 {
		um := map[string]int64{}
		if v := resp.Usage["input"]; v > 0 {
			um["promptTokenCount"] = v
		}
		if v := resp.Usage["output"]; v > 0 {
			um["candidatesTokenCount"] = v
		}
		if v := resp.Usage["cache_read"]; v > 0 {
			um["cachedContentTokenCount"] = v
		}
		if v := resp.Usage["reasoning"]; v > 0 {
			um["thoughtsTokenCount"] = v
		}
		out["usageMetadata"] = um
	}

	if resp.Model != "" {
		out["modelVersion"] = resp.Model
	}

	return json.Marshal(out)
}

// ---- NewToCanonicalStream ----

// NewToCanonicalStream returns a stateful per-stream function that converts
// Gemini SSE chunks (streamGenerateContent?alt=sse) into canonical SSE chunks.
func (GeminiTranslator) NewToCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &geminiToCanonicalStream{}
	return s.translate
}

// ---- NewFromCanonicalStream ----

// NewFromCanonicalStream returns a stateful per-stream function that converts
// canonical SSE chunks into Gemini SSE frames.
func (GeminiTranslator) NewFromCanonicalStream() func(chunk []byte) ([]byte, error) {
	s := &canonicalToGeminiStream{}
	return s.translate
}

// ---- stream: Gemini → canonical ----

type geminiToCanonicalStream struct {
	responseID       string
	model            string
	created          int64
	lifecycleEmitted bool
	nextIndex        int
	// per-part accumulation (Gemini streams part-by-part within one candidate)
	currentItemID     string
	currentItemType   v1.ItemType
	currentIndex      int
	textBuf           strings.Builder
	argsBuf           strings.Builder
	thinkBuf          strings.Builder
	currentFCName     string
	currentThoughtSig string // thoughtSignature for the current item, if any
	// sawFunctionCall records whether any function_call part appeared across
	// the whole stream, so the terminal completion reports finish_reason
	// tool_calls even though Gemini's per-frame finishReason is usually STOP.
	sawFunctionCall bool
}

func (s *geminiToCanonicalStream) translate(chunk []byte) ([]byte, error) {
	_, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	var gr geminiResponse
	if err := json.Unmarshal(data, &gr); err != nil {
		// Try error shape.
		var errResp struct {
			Error struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
				Status  string `json:"status"`
			} `json:"error"`
		}
		if jerr := json.Unmarshal(data, &errResp); jerr == nil && errResp.Error.Message != "" {
			errData, _ := json.Marshal(v1.ErrorEvent{
				Code:    errResp.Error.Status,
				Message: errResp.Error.Message,
			})
			return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventError, Data: errData}}), nil
		}
		return nil, fmt.Errorf("gemini stream: %w", err)
	}

	var out []byte

	// Emit generation.created on first frame.
	if !s.lifecycleEmitted {
		s.created = time.Now().Unix()
		s.responseID = fmt.Sprintf("gemini-%d", s.created)
		s.model = gr.ModelVersion
		s.lifecycleEmitted = true

		createdData, _ := json.Marshal(v1.GenerationCreatedEvent{
			ID:    s.responseID,
			Model: s.model,
		})
		out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventGenerationCreated, Data: createdData}})...)
	}

	if len(gr.Candidates) == 0 {
		return out, nil
	}
	cand := gr.Candidates[0]
	if cand.Content == nil {
		// Terminal frame with no content but finishReason/usage.
		if cand.FinishReason != "" || gr.UsageMetadata != nil {
			out = append(out, s.emitCompletion(cand.FinishReason, gr.UsageMetadata)...)
		}
		return out, nil
	}

	for _, p := range cand.Content.Parts {
		if p.FunctionCall != nil {
			// Close any open text/thought item.
			out = append(out, s.closeCurrentItem()...)

			idx := s.nextIndex
			s.nextIndex++
			itemID := fmt.Sprintf("fc_%d", idx)
			s.currentItemID = itemID
			s.currentItemType = v1.ItemTypeFunctionCall
			s.currentIndex = idx
			s.currentFCName = p.FunctionCall.Name
			s.currentThoughtSig = p.ThoughtSignature
			s.sawFunctionCall = true
			s.argsBuf.Reset()

			argsStr := "{}"
			if len(p.FunctionCall.Args) > 0 {
				argsStr = string(p.FunctionCall.Args)
			}
			s.argsBuf.WriteString(argsStr)

			startData, _ := json.Marshal(v1.ItemStartedEvent{
				ItemID:   itemID,
				ItemType: v1.ItemTypeFunctionCall,
				Index:    idx,
				Name:     p.FunctionCall.Name,
			})
			out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemStarted, Data: startData}})...)

			deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
				ItemID: itemID,
				Index:  idx,
				Kind:   v1.DeltaKindArguments,
				Delta:  argsStr,
			})
			out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemDelta, Data: deltaData}})...)

			// Function call arrives complete in one frame.
			out = append(out, s.closeCurrentItem()...)

		} else if p.Text != "" && p.Thought {
			// Reasoning part.
			if s.currentItemType != v1.ItemTypeReasoning {
				out = append(out, s.closeCurrentItem()...)
				idx := s.nextIndex
				s.nextIndex++
				itemID := fmt.Sprintf("rs_%d", idx)
				s.currentItemID = itemID
				s.currentItemType = v1.ItemTypeReasoning
				s.currentIndex = idx
				s.currentThoughtSig = p.ThoughtSignature
				s.thinkBuf.Reset()

				startData, _ := json.Marshal(v1.ItemStartedEvent{
					ItemID:   itemID,
					ItemType: v1.ItemTypeReasoning,
					Index:    idx,
				})
				out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemStarted, Data: startData}})...)
			}
			s.thinkBuf.WriteString(p.Text)
			deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
				ItemID: s.currentItemID,
				Index:  s.currentIndex,
				Kind:   v1.DeltaKindReasoning,
				Delta:  p.Text,
			})
			out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemDelta, Data: deltaData}})...)

		} else if p.Text != "" {
			// Text part.
			if s.currentItemType != v1.ItemTypeMessage {
				out = append(out, s.closeCurrentItem()...)
				idx := s.nextIndex
				s.nextIndex++
				itemID := fmt.Sprintf("msg_%d", idx)
				s.currentItemID = itemID
				s.currentItemType = v1.ItemTypeMessage
				s.currentIndex = idx
				s.textBuf.Reset()

				startData, _ := json.Marshal(v1.ItemStartedEvent{
					ItemID:   itemID,
					ItemType: v1.ItemTypeMessage,
					Index:    idx,
				})
				out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemStarted, Data: startData}})...)
			}
			s.textBuf.WriteString(p.Text)
			deltaData, _ := json.Marshal(v1.ItemDeltaEvent{
				ItemID: s.currentItemID,
				Index:  s.currentIndex,
				Kind:   v1.DeltaKindText,
				Delta:  p.Text,
			})
			out = append(out, marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemDelta, Data: deltaData}})...)
		}
	}

	// Terminal frame: finishReason present means stream is done.
	if cand.FinishReason != "" {
		out = append(out, s.closeCurrentItem()...)
		out = append(out, s.emitCompletion(cand.FinishReason, gr.UsageMetadata)...)
	}

	return out, nil
}

func (s *geminiToCanonicalStream) closeCurrentItem() []byte {
	if s.currentItemID == "" {
		return nil
	}
	var completedItem v1.Item
	switch s.currentItemType {
	case v1.ItemTypeMessage:
		completedItem = &v1.Message{
			ID:      s.currentItemID,
			Role:    v1.RoleAssistant,
			Status:  v1.StatusCompleted,
			Content: []v1.Part{&v1.OutputTextPart{Text: s.textBuf.String()}},
		}
	case v1.ItemTypeFunctionCall:
		fc := &v1.FunctionCall{
			ID:        s.currentItemID,
			CallID:    geminiCallID(s.currentFCName, s.currentIndex),
			Name:      s.currentFCName,
			Arguments: s.argsBuf.String(),
			Status:    v1.StatusCompleted,
		}
		if s.currentThoughtSig != "" {
			fc.ProviderData = thoughtSignatureJSON(s.currentThoughtSig)
		}
		completedItem = fc
	case v1.ItemTypeReasoning:
		r := &v1.Reasoning{
			ID:      s.currentItemID,
			Content: s.thinkBuf.String(),
			Summary: []v1.SummaryText{{Text: s.thinkBuf.String()}},
			Status:  v1.StatusCompleted,
		}
		if s.currentThoughtSig != "" {
			r.ProviderData = thoughtSignatureJSON(s.currentThoughtSig)
		}
		completedItem = r
	default:
		return nil
	}
	s.currentThoughtSig = ""

	completedData, _ := json.Marshal(v1.ItemCompletedEvent{
		ItemID: s.currentItemID,
		Index:  s.currentIndex,
		Item:   completedItem,
	})
	s.currentItemID = ""
	s.currentItemType = ""
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventItemCompleted, Data: completedData}})
}

func (s *geminiToCanonicalStream) emitCompletion(finishReason string, um *usageMetadata) []byte {
	status, finish, _ := geminiFinishReasonToCanonical(finishReason, s.sawFunctionCall)

	gen := v1.GenerationCompletedEvent{
		ID:           s.responseID,
		Status:       status,
		FinishReason: finish,
		Usage:        geminiUsageToTokens(um),
	}
	completedData, _ := json.Marshal(gen)
	return marshalCanonFrames([]v1.SSEFrame{{Event: v1.EventGenerationCompleted, Data: completedData}})
}

// ---- stream: canonical → Gemini ----

type canonicalToGeminiStream struct {
	responseID string
	model      string
	// accumulated parts for the current candidate
	parts        []geminiPart
	finishReason string
	// current function-call item being assembled. Gemini does not stream
	// partial function args (unlike canonical's arguments deltas), so we
	// buffer them and emit one complete functionCall frame on item.completed.
	inFunctionCall bool
	fcName         string
	fcArgs         strings.Builder
}

func (s *canonicalToGeminiStream) translate(chunk []byte) ([]byte, error) {
	event, data, ok := v1.ParseSSEChunk(chunk)
	if !ok {
		return nil, nil
	}

	switch event {
	case v1.EventGenerationCreated:
		var e v1.GenerationCreatedEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: generation.created: %w", err)
		}
		s.responseID = e.ID
		s.model = e.Model
		return nil, nil // Gemini has no session-open frame

	case v1.EventItemStarted:
		var e v1.ItemStartedEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: item.started: %w", err)
		}
		if e.ItemType == v1.ItemTypeFunctionCall {
			// Begin buffering a function call; args arrive as deltas and the
			// complete functionCall is emitted on item.completed.
			s.inFunctionCall = true
			s.fcName = e.Name
			s.fcArgs.Reset()
		}
		return nil, nil

	case v1.EventItemDelta:
		var e v1.ItemDeltaEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: item.delta: %w", err)
		}
		// Text and reasoning stream incrementally (Gemini streams text parts).
		// Arguments are buffered, not streamed — Gemini emits functionCall
		// whole, so accumulate and flush on item.completed.
		var p geminiPart
		switch e.Kind {
		case v1.DeltaKindText:
			p = geminiPart{Text: e.Delta}
		case v1.DeltaKindReasoning:
			p = geminiPart{Text: e.Delta, Thought: true}
		case v1.DeltaKindArguments:
			s.fcArgs.WriteString(e.Delta)
			return nil, nil
		default:
			return nil, nil
		}
		frame := geminiResponse{
			Candidates: []candidate{{
				Content: &geminiContent{Role: "model", Parts: []geminiPart{p}},
				Index:   0,
			}},
			ModelVersion: s.model,
		}
		return geminiSSEBytes(frame)

	case v1.EventItemCompleted:
		if !s.inFunctionCall {
			// Text/reasoning content already streamed via deltas.
			return nil, nil
		}
		// Flush the buffered function call as one complete frame. The
		// accumulated args are a complete JSON object string; embed verbatim
		// (fall back to {} if empty or invalid).
		s.inFunctionCall = false
		args := json.RawMessage("{}")
		if a := s.fcArgs.String(); a != "" && json.Valid([]byte(a)) {
			args = json.RawMessage(a)
		}
		frame := geminiResponse{
			Candidates: []candidate{{
				Content: &geminiContent{Role: "model", Parts: []geminiPart{{
					FunctionCall: &geminiFC{Name: s.fcName, Args: args},
				}}},
				Index: 0,
			}},
			ModelVersion: s.model,
		}
		return geminiSSEBytes(frame)

	case v1.EventGenerationCompleted:
		var e v1.GenerationCompletedEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: generation.completed: %w", err)
		}
		finReason := canonicalFinishReasonToGemini(e.FinishReason, nil)
		frame := map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": []any{}},
				"finishReason": finReason,
				"index":        0,
			}},
			"modelVersion": s.model,
		}
		if len(e.Usage) > 0 {
			um := map[string]int64{}
			if v := e.Usage["input"]; v > 0 {
				um["promptTokenCount"] = v
			}
			if v := e.Usage["output"]; v > 0 {
				um["candidatesTokenCount"] = v
			}
			if v := e.Usage["cache_read"]; v > 0 {
				um["cachedContentTokenCount"] = v
			}
			if v := e.Usage["reasoning"]; v > 0 {
				um["thoughtsTokenCount"] = v
			}
			frame["usageMetadata"] = um
		}
		b, err := json.Marshal(frame)
		if err != nil {
			return nil, err
		}
		return geminiSSEBytesRaw(b), nil

	case v1.EventError:
		var e v1.ErrorEvent
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("canonical→gemini: error: %w", err)
		}
		errFrame := map[string]any{
			"error": map[string]any{
				"code":    500,
				"message": e.Message,
				"status":  e.Code,
			},
		}
		b, _ := json.Marshal(errFrame)
		return geminiSSEBytesRaw(b), nil

	default:
		return nil, nil
	}
}

// ---- shared helpers ----

func geminiSSEBytes(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return geminiSSEBytesRaw(b), nil
}

func geminiSSEBytesRaw(data []byte) []byte {
	var sb strings.Builder
	sb.WriteString("data: ")
	sb.Write(data)
	sb.WriteString("\n\n")
	return []byte(sb.String())
}

func marshalCanonFrames(frames []v1.SSEFrame) []byte {
	var buf []byte
	for _, f := range frames {
		buf = append(buf, f.Bytes()...)
	}
	return buf
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
			for _, p := range c.Parts {
				if p.FunctionResponse != nil {
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

// canonicalItemsToGemini converts canonical []v1.Item to Gemini contents.
// System/developer role messages are returned separately as extraSys text.
func canonicalItemsToGemini(items []v1.Item) ([]geminiContent, string, error) {
	var contents []geminiContent
	var sysExtra []string

	// Gemini requires strict user/model alternation. We flush pending
	// function calls and results into the appropriate role content.
	var pendingFCs []v1.FunctionCall
	var pendingFCOs []v1.FunctionCallOutput

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
			var respRaw json.RawMessage
			if json.Unmarshal([]byte(resp), &respRaw) != nil {
				// Wrap plain string in an object.
				b, _ := json.Marshal(map[string]string{"output": resp})
				respRaw = b
			}
			parts = append(parts, geminiPart{FunctionResponse: &geminiFR{
				Name:     geminiFuncNameFromCallID(fco.CallID),
				Response: respRaw,
			}})
		}
		contents = append(contents, geminiContent{Role: "user", Parts: parts})
		pendingFCOs = pendingFCOs[:0]
	}

	for _, item := range items {
		switch v := item.(type) {
		case *v1.Message:
			flushFCs()
			flushFCOs()

			switch v.Role {
			case v1.RoleSystem, v1.RoleDeveloper:
				var sb strings.Builder
				for _, p := range v.Content {
					switch tp := p.(type) {
					case *v1.TextPart:
						sb.WriteString(tp.Text)
					case *v1.OutputTextPart:
						sb.WriteString(tp.Text)
					}
				}
				if s := sb.String(); s != "" {
					sysExtra = append(sysExtra, s)
				}

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
			flushFCOs()
			pendingFCs = append(pendingFCs, *v)

		case *v1.FunctionCallOutput:
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

// Gemini has no per-call IDs — it matches tool results to calls by function
// name. Canonical (and OpenAI/Anthropic clients downstream) require a unique
// CallID per call, so we synthesize CallID = name + callIDSep + index and
// strip it back to the bare name when emitting a functionResponse upstream.
// callIDSep is chosen to never collide with a real function name.
const callIDSep = "__relay_call_"

func geminiCallID(name string, idx int) string {
	return fmt.Sprintf("%s%s%d", name, callIDSep, idx)
}

// geminiFuncNameFromCallID recovers the bare Gemini function name from a
// CallID we synthesized (or returns the input unchanged if it carries no
// suffix — e.g. a CallID minted by a different inbound adapter).
func geminiFuncNameFromCallID(callID string) string {
	if i := strings.LastIndex(callID, callIDSep); i >= 0 {
		return callID[:i]
	}
	return callID
}

// geminiFinishReasonToCanonical maps a Gemini finishReason string to canonical status/finish/incomplete.
func geminiFinishReasonToCanonical(reason string, hasFunctionCall bool) (v1.Status, v1.FinishReason, *v1.IncompleteDetails) {
	if hasFunctionCall {
		return v1.StatusCompleted, v1.FinishReasonToolCalls, nil
	}
	switch reason {
	case "STOP", "":
		return v1.StatusCompleted, v1.FinishReasonStop, nil
	case "MAX_TOKENS":
		return v1.StatusIncomplete, v1.FinishReasonLength, &v1.IncompleteDetails{Reason: "max_tokens"}
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		// Content blocked by Gemini's safety/policy filters. Must NOT look like
		// a normal STOP — safety auditing, retry, and billing classification
		// downstream branch on content_filter.
		return v1.StatusIncomplete, v1.FinishReasonContentFilter, &v1.IncompleteDetails{Reason: "content_filter"}
	case "MALFORMED_FUNCTION_CALL":
		// Gemini failed to emit a valid function call — a generation failure,
		// not a clean stop.
		return v1.StatusFailed, v1.FinishReasonStop, &v1.IncompleteDetails{Reason: "malformed_function_call"}
	case "LANGUAGE":
		// Unsupported language — treated as a content filter (output withheld).
		return v1.StatusIncomplete, v1.FinishReasonContentFilter, &v1.IncompleteDetails{Reason: "unsupported_language"}
	default:
		// Unknown/future reason: surface as incomplete with the raw reason
		// rather than silently masquerading as a successful stop.
		return v1.StatusIncomplete, v1.FinishReasonStop, &v1.IncompleteDetails{Reason: "gemini:" + reason}
	}
}

// canonicalFinishReasonToGemini maps canonical finish_reason + incomplete_details back to a Gemini finishReason string.
func canonicalFinishReasonToGemini(reason v1.FinishReason, incomplete *v1.IncompleteDetails) string {
	if incomplete != nil && incomplete.Reason == "max_tokens" {
		return "MAX_TOKENS"
	}
	switch reason {
	case v1.FinishReasonStop:
		return "STOP"
	case v1.FinishReasonLength:
		return "MAX_TOKENS"
	case v1.FinishReasonContentFilter:
		return "SAFETY"
	case v1.FinishReasonToolCalls:
		return "STOP" // Gemini uses STOP even when the last action was a function call
	default:
		return "STOP"
	}
}

// thoughtSignatureJSON encodes a thoughtSignature value into the provider_data
// JSON shape used on v1.FunctionCall and v1.Reasoning items.
func thoughtSignatureJSON(sig string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"thoughtSignature": sig})
	return b
}

// thoughtSignatureFrom extracts the thoughtSignature from a provider_data blob,
// returning "" if absent or malformed.
func thoughtSignatureFrom(pd json.RawMessage) string {
	if len(pd) == 0 {
		return ""
	}
	var v struct {
		ThoughtSignature string `json:"thoughtSignature"`
	}
	if err := json.Unmarshal(pd, &v); err != nil {
		return ""
	}
	return v.ThoughtSignature
}

// resolveModelOpts picks the ModelOpts for the given model name following the
// fallback rules: exact key match → single-entry fallback → nil.
func resolveModelOpts(modelConfig map[string]*v1.ModelOpts, model string) *v1.ModelOpts {
	if o, ok := modelConfig[model]; ok {
		return o
	}
	if len(modelConfig) == 1 {
		for _, o := range modelConfig {
			return o
		}
	}
	return nil
}
