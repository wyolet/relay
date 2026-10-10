package adapters_test

import (
	"encoding/json"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// ownProviderData is the opaque blob a shape writes onto items of its own
// parse, so the full request exercises the same-vendor path rule 8 promises
// to keep. A nil slot gets a foreign blob instead.
type ownProviderData struct {
	reasoning    json.RawMessage
	functionCall json.RawMessage
}

var providerDataByShape = map[string]ownProviderData{
	"openai-chat":      {reasoning: raw(`{"cc_reasoning_field":"reasoning_content"}`)},
	"openai-responses": {reasoning: raw(`{"encrypted_content":"enc-1","id":"rs_1"}`)},
	"anthropic":        {reasoning: raw(`{"type":"thinking","thinking":"raw thought","signature":"sig-1"}`)},
	"gemini": {
		reasoning:    raw(`{"thoughtSignature":"sig-1"}`),
		functionCall: raw(`{"thoughtSignature":"sig-2"}`),
	},
}

var foreignProviderData = raw(`{"foreign":"blob"}`)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func ptr[T any](v T) *T { return &v }

// fullRequest builds a canonical request with every field set to a value that
// is valid on its own. Parts and tools some shapes reject outright are left to
// rejectedFeatures, which checks the rejection is loud.
func fullRequest(shapeName string) *v1.Request {
	pd := providerDataByShape[shapeName]
	or := func(own json.RawMessage) json.RawMessage {
		if own != nil {
			return own
		}
		return foreignProviderData
	}
	return &v1.Request{
		Model:        v1.ModelRefs{"m"},
		Instructions: "Be terse.",
		ModelConfig: map[string]*v1.ModelOpts{"m": {
			Sampling: &v1.SamplingParams{
				Temperature:      ptr(0.4),
				TopP:             ptr(0.9),
				TopK:             ptr(40),
				MaxTokens:        ptr(2048),
				Stop:             []string{"END"},
				Seed:             ptr(7),
				FrequencyPenalty: ptr(0.1),
				PresencePenalty:  ptr(0.2),
			},
			Reasoning: &v1.ReasoningConfig{Effort: "high", Summary: "auto", BudgetTokens: ptr(1500)},
			Output: &v1.OutputConfig{
				Format: &v1.Format{
					Type:        "json_schema",
					Name:        "answer",
					Description: "The answer object.",
					Schema:      raw(`{"type":"object","properties":{"a":{"type":"string"}}}`),
					Strict:      ptr(true),
				},
				Verbosity: "low",
			},
		}},
		Tools: &v1.ToolsConfig{
			Definitions: v1.Tools{&v1.FunctionTool{
				Name:         "lookup",
				Description:  "Look a term up.",
				Parameters:   raw(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
				Strict:       ptr(true),
				ProviderData: foreignProviderData,
			}},
			Choice:   &v1.ToolChoice{Mode: "function", FunctionName: "lookup"},
			Parallel: ptr(false),
		},
		CacheConfig: &v1.CacheConfig{Instructions: true, Tools: true, Key: "conv-1", TTL: "24h"},
		OutputMode:  v1.OutputModeStream,
		User:        "user-1",
		Metadata:    map[string]string{"team": "core"},
		Extensions: map[string]json.RawMessage{
			"openai.logit_bias":   raw(`{"50256":-100}`),
			"openai.logprobs":     raw(`true`),
			"openai.top_logprobs": raw(`2`),
			"openai.service_tier": raw(`"priority"`),
			"openai.store":        raw(`false`),
			"acme.trace":          raw(`"t-1"`),
		},
		Input: []v1.Item{
			&v1.Message{
				ID:           "msg_u1",
				Status:       v1.StatusCompleted,
				Role:         v1.RoleUser,
				CacheConfig:  &v1.ItemCacheConfig{Anchor: true},
				ProviderData: foreignProviderData,
				Content: []v1.Part{
					&v1.TextPart{Text: "What is in these files?"},
					&v1.ImagePart{ImageURL: "https://example.com/cat.png", Detail: "high"},
					&v1.FilePart{FileURL: "https://example.com/a.pdf", Filename: "a.pdf", MediaType: "application/pdf"},
					&v1.FilePart{FileData: "JVBERi0xLjQK", Filename: "b.pdf", MediaType: "application/pdf"},
				},
			},
			&v1.Reasoning{
				ID:           "rs_1",
				Summary:      []v1.SummaryText{{Text: "Weighing the files."}},
				Content:      "raw thought",
				Status:       v1.StatusCompleted,
				ProviderData: or(pd.reasoning),
			},
			&v1.Message{
				ID:     "msg_a1",
				Status: v1.StatusCompleted,
				Role:   v1.RoleAssistant,
				Content: []v1.Part{&v1.OutputTextPart{
					Text: "Checking.",
					Annotations: []v1.Annotation{
						&v1.URLCitationAnnotation{StartIndex: 0, EndIndex: 8, URL: "https://example.com/src", Title: "Source"},
						&v1.TextCitationAnnotation{StartIndex: 0, EndIndex: 4},
					},
				}},
			},
			&v1.FunctionCall{
				ID:           "fc_1",
				CallID:       "call_1",
				Name:         "lookup",
				Arguments:    `{"q":"cat"}`,
				Status:       v1.StatusCompleted,
				ProviderData: or(pd.functionCall),
			},
			&v1.FunctionCallOutput{CallID: "call_1", Output: "a cat"},
			&v1.FunctionCall{CallID: "call_2", Name: "lookup", Arguments: `{"q":"pdf"}`},
			&v1.FunctionCallOutput{CallID: "call_2", Content: []v1.Part{
				&v1.TextPart{Text: "page one"},
				&v1.ImagePart{ImageURL: "data:image/png;base64,iVBORw0KGgo="},
			}},
			&v1.Message{Role: v1.RoleDeveloper, Content: []v1.Part{&v1.TextPart{Text: "Answer in English."}}},
			&v1.Message{Role: v1.RoleSystem, Hoist: true, Content: []v1.Part{&v1.TextPart{Text: "Never name the tools."}}},
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "Summarize."}}},
		},
	}
}

// rejectedFeatures are canonical inputs at least one shape refuses with an
// error rather than serializing. Each one is checked on its own against every
// shape: it must either survive the round trip or fail loudly.
var rejectedFeatures = map[string]func(*v1.Request){
	"file part by id": func(r *v1.Request) {
		r.Input = []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{
			&v1.TextPart{Text: "Read this."},
			&v1.FilePart{FileID: "file-abc", Filename: "c.pdf"},
		}}}
	},
	"server tool": func(r *v1.Request) {
		r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{&v1.ServerTool{Name: "web_search"}}}
	},
	"mcp tool": func(r *v1.Request) {
		r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{&v1.MCPTool{Name: "docs", ServerURL: "https://mcp.example.com", Headers: map[string]string{"X-Key": "k"}}}}
	},
}

// minimalRequest is the base each rejected feature is layered onto.
func minimalRequest() *v1.Request {
	return &v1.Request{
		Model: v1.ModelRefs{"m"},
		Input: []v1.Item{&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: "hi"}}}},
	}
}
