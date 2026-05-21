package responses

import "encoding/json"

// Response is the non-streaming response object from POST /v1/responses.
// Required fields per the OpenAI spec are always serialized (no omitempty).
// Request-echo fields (Instructions, Tools, ToolChoice, ParallelToolCalls,
// Metadata, Temperature, TopP) must be populated by the translators from the
// original *Request so the body matches the spec's required-field list.
type Response struct {
	ID           string       `json:"id"`
	Object       string       `json:"object"`       // always "response"
	CreatedAt    int64        `json:"created_at"`   // unix seconds
	Model        string       `json:"model"`
	Status       Status       `json:"status"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Output       []Item       `json:"output"`
	Usage        *Usage       `json:"usage,omitempty"`

	// Required spec fields that may be null — no omitempty so they serialize.
	Error             *Error             `json:"error"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details"`
	Instructions      *string            `json:"instructions"`
	Temperature       *float64           `json:"temperature"`
	TopP              *float64           `json:"top_p"`

	// Required spec fields that must never serialize as null.
	// Tools serializes as [] when nil; Metadata as {}; ToolChoice as "auto".
	// ParallelToolCalls is a plain bool — default true.
	ParallelToolCalls bool              `json:"parallel_tool_calls"`
	Tools             Tools             `json:"-"`             // handled in MarshalJSON
	Metadata          map[string]string `json:"-"`             // handled in MarshalJSON
	ToolChoiceRaw     json.RawMessage   `json:"-"`             // handled in MarshalJSON
}

// MarshalJSON emits the Response ensuring Tools→[], Metadata→{}, and
// ToolChoice defaults to "auto". Using an alias breaks the MarshalJSON cycle.
func (r Response) MarshalJSON() ([]byte, error) {
	type Alias Response // avoids infinite recursion

	toolsJSON := json.RawMessage("[]")
	if len(r.Tools) > 0 {
		b, err := json.Marshal(r.Tools)
		if err != nil {
			return nil, err
		}
		toolsJSON = b
	}

	metaJSON := json.RawMessage("{}")
	if len(r.Metadata) > 0 {
		b, err := json.Marshal(r.Metadata)
		if err != nil {
			return nil, err
		}
		metaJSON = b
	}

	tcJSON := r.ToolChoiceRaw
	if len(tcJSON) == 0 {
		tcJSON = json.RawMessage(`"auto"`)
	}

	// Embed the alias (all fields except the overridden ones), then inject
	// tools, metadata, tool_choice via a wrapper struct.
	type wire struct {
		Alias
		Tools      json.RawMessage `json:"tools"`
		Metadata   json.RawMessage `json:"metadata"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	return json.Marshal(wire{
		Alias:      Alias(r),
		Tools:      toolsJSON,
		Metadata:   metaJSON,
		ToolChoice: tcJSON,
	})
}

// Usage carries token counts for the response.
// InputTokensDetails and OutputTokensDetails are always serialized per spec.
type Usage struct {
	InputTokens         int         `json:"input_tokens"`
	OutputTokens        int         `json:"output_tokens"`
	TotalTokens         int         `json:"total_tokens"`
	InputTokensDetails  InputDeets  `json:"input_tokens_details"`
	OutputTokensDetails OutputDeets `json:"output_tokens_details"`
}

// InputDeets holds per-category input token counts.
type InputDeets struct {
	CachedTokens int `json:"cached_tokens"`
}

// OutputDeets holds per-category output token counts.
type OutputDeets struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Error is an API-level error embedded in a response object.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// IncompleteDetails explains why a response was not completed.
type IncompleteDetails struct {
	Reason string `json:"reason,omitempty"`
}

// EchoRequest copies the request-echo fields from req into resp so the
// response body satisfies the OpenAI spec's required-field list. Call this
// after building the core response fields (id, status, output, usage).
func EchoRequest(resp *Response, req *Request) {
	if req == nil {
		return
	}

	// Instructions: null when absent in the request.
	if req.Instructions != "" {
		s := req.Instructions
		resp.Instructions = &s
	}

	resp.Temperature = req.Temperature
	resp.TopP = req.TopP

	// parallel_tool_calls: spec default is true; honour explicit false.
	if req.ParallelToolCalls != nil {
		resp.ParallelToolCalls = *req.ParallelToolCalls
	} else {
		resp.ParallelToolCalls = true
	}

	// Tools: use request tools if present, else stay nil (serialises as []).
	if len(req.Tools) > 0 {
		resp.Tools = req.Tools
	}

	// tool_choice: marshal the request value, fall back to "auto".
	if req.ToolChoice != nil {
		if b, err := json.Marshal(req.ToolChoice); err == nil {
			resp.ToolChoiceRaw = b
		}
	}

	// metadata: copy if present, else stay nil (serialises as {}).
	if len(req.Metadata) > 0 {
		resp.Metadata = req.Metadata
	}
}
