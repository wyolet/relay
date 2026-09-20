package openai

import (
	"encoding/json"
	"fmt"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// responsesRequestToCanonical maps a *ResponsesRequest to a canonical *v1.Request.
func responsesRequestToCanonical(req *ResponsesRequest) (*v1.Request, error) {
	cr := &v1.Request{
		Model:        v1.ModelRefs{req.Model},
		Instructions: req.Instructions,
		User:         req.User,
		Metadata:     req.Metadata,
	}

	cr.CacheConfig = openaiCacheConfigFromWire(req.PromptCacheKey, req.PromptCacheRetention)

	if req.Stream != nil && *req.Stream {
		cr.OutputMode = v1.OutputModeStream
	} else {
		cr.OutputMode = v1.OutputModeSync
	}

	// Build canonical input from Responses items.
	input := make([]v1.Item, 0, len(req.Input))
	for _, item := range req.Input {
		ci, err := responsesItemToCanonical(item)
		if err != nil {
			return nil, fmt.Errorf("input item: %w", err)
		}
		if ci != nil {
			input = append(input, ci)
		}
	}
	cr.Input = input

	// Build ModelOpts.
	opts := &v1.ModelOpts{}
	hasOpts := false

	// Sampling params.
	if req.Temperature != nil || req.TopP != nil || req.MaxOutputTokens != nil || req.TopK != nil ||
		len(req.StopSequences) > 0 {
		sp := &v1.SamplingParams{}
		sp.Temperature = req.Temperature
		sp.TopP = req.TopP
		if req.MaxOutputTokens != nil {
			sp.MaxTokens = req.MaxOutputTokens
		}
		sp.Stop = req.StopSequences
		opts.Sampling = sp
		hasOpts = true
	}

	// Tools.
	if len(req.Tools) > 0 {
		tc := &v1.ToolsConfig{}
		for _, t := range req.Tools {
			ft, ok := t.(*ResponsesFunctionTool)
			if !ok {
				// canonical: hosted-tool definition (web_search, mcp, …) dropped —
				// not expressible to a non-OpenAI upstream. Skip rather than 400
				// the whole request (rule 11: annotated, not silent).
				continue
			}
			params := ft.Parameters
			if params == nil {
				params = json.RawMessage(`{}`)
			}
			tc.Definitions = append(tc.Definitions, &v1.FunctionTool{
				Name:        ft.Name,
				Description: ft.Description,
				Parameters:  params,
				Strict:      ft.Strict,
			})
		}
		tc.Parallel = req.ParallelToolCalls
		if req.ToolChoice != nil {
			choice := &v1.ToolChoice{
				Mode:         req.ToolChoice.Mode,
				FunctionName: req.ToolChoice.FunctionName,
			}
			tc.Choice = choice
		}
		cr.Tools = tc
	}

	// canonical: max_tool_calls dropped — no canonical field for a tool-call cap.
	// Honored only on the byte-pass path; cross-shape it cannot be expressed.
	_ = req.MaxToolCalls

	// Reasoning.
	if req.Reasoning != nil && (req.Reasoning.Effort != "" || req.Reasoning.Summary != "") {
		opts.Reasoning = &v1.ReasoningConfig{
			Effort:  req.Reasoning.Effort,
			Summary: req.Reasoning.Summary,
		}
		hasOpts = true
	}

	// Output format + verbosity.
	if req.Text != nil && (req.Text.Format != nil || req.Text.Verbosity != "") {
		oc := &v1.OutputConfig{Verbosity: req.Text.Verbosity}
		if f := req.Text.Format; f != nil {
			oc.Format = &v1.Format{
				Type:        f.Type,
				Name:        f.Name,
				Description: f.Description,
				Schema:      f.Schema,
				Strict:      f.Strict,
			}
		}
		opts.Output = oc
		hasOpts = true
	}

	if hasOpts {
		cr.ModelConfig = map[string]*v1.ModelOpts{req.Model: opts}
	}

	return cr, nil
}

// canonicalToResponsesRequest maps a canonical *v1.Request back to a *ResponsesRequest.
// Used for SerializeRequest and for echo fields in SerializeResponse.
func canonicalToResponsesRequest(req *v1.Request) (*ResponsesRequest, error) {
	if len(req.Model) == 0 {
		return nil, fmt.Errorf("canonical request has no model")
	}
	model := req.Model[0]

	// Hoist-flagged system items merge into instructions; non-hoisted ones
	// stay positional — Responses supports system/developer items natively.
	// (SerializeRequest drops the hoisted items from input.)
	_, hoistedSys := v1.SplitHoistedSystem(req.Input)
	instructions := req.Instructions
	if hoistedSys != "" {
		if instructions != "" {
			instructions = instructions + "\n" + hoistedSys
		} else {
			instructions = hoistedSys
		}
	}

	rreq := &ResponsesRequest{
		Model:        model,
		Instructions: instructions,
		User:         req.User,
		Metadata:     req.Metadata,
	}

	if req.CacheConfig != nil {
		rreq.PromptCacheKey = req.CacheConfig.Key
		rreq.PromptCacheRetention = openaiCacheRetention(req.CacheConfig)
	}

	if req.OutputMode == v1.OutputModeStream {
		t := true
		rreq.Stream = &t
	}

	opts := req.ModelConfig[model]
	if opts != nil {
		if opts.Sampling != nil {
			s := opts.Sampling
			rreq.Temperature = s.Temperature
			rreq.TopP = s.TopP
			rreq.MaxOutputTokens = s.MaxTokens
			// canonical: stop_sequences dropped — the Responses API has no
			// stop-sequence parameter (Chat Completions only); emitting it 400s.
			// canonical: Seed has no Responses wire equivalent — dropped
			// canonical: FrequencyPenalty has no Responses wire equivalent — dropped
			// canonical: PresencePenalty has no Responses wire equivalent — dropped
			// TopK not in v1 canonical sampling params — omit
		}
		if opts.Reasoning != nil {
			rc := &ResponsesReasoningConfig{Effort: opts.Reasoning.Effort}
			// R-5: map canonical Summary to Responses reasoning.summary field.
			if opts.Reasoning.Summary != "" {
				rc.Summary = opts.Reasoning.Summary
			}
			// canonical: BudgetTokens has no Responses wire equivalent — dropped
			rreq.Reasoning = rc
		}
		if opts.Output != nil && (opts.Output.Format != nil || opts.Output.Verbosity != "") {
			tc := &ResponsesTextConfig{Verbosity: opts.Output.Verbosity}
			if f := opts.Output.Format; f != nil {
				tc.Format = &ResponsesFormat{
					Type:        f.Type,
					Name:        f.Name,
					Description: f.Description,
					Schema:      f.Schema,
					Strict:      f.Strict,
				}
			}
			rreq.Text = tc
		}
	}

	// Tools are task-level (req.Tools), shared across models — not per-model.
	if tc := req.Tools; tc != nil {
		for _, tool := range tc.Definitions {
			ft, ok := tool.(*v1.FunctionTool)
			if !ok {
				continue
			}
			params := ft.Parameters
			if params == nil {
				params = json.RawMessage(`{}`)
			}
			rreq.Tools = append(rreq.Tools, &ResponsesFunctionTool{
				Name:        ft.Name,
				Description: ft.Description,
				Parameters:  params,
				Strict:      ft.Strict,
			})
		}
		rreq.ParallelToolCalls = tc.Parallel
		if tc.Choice != nil {
			rreq.ToolChoice = &ResponsesToolChoice{
				Mode:         tc.Choice.Mode,
				FunctionName: tc.Choice.FunctionName,
			}
		}
	}

	return rreq, nil
}
