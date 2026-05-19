package openai

import (
	"encoding/json"
	"errors"
	"fmt"
)

// FullChatRequest mirrors POST /v1/chat/completions request body.
// Polymorphic fields (Content, Stop, ToolChoice) are kept as json.RawMessage
// so unknown variants pass through to upstream untouched.
// Optional numerics use pointers so zero is distinguishable from absence.
type FullChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`

	// Inherited from ModelResponseProperties via CreateModelResponseProperties
	Metadata    map[string]string `json:"metadata,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
	TopP        *float64          `json:"top_p,omitempty"`
	User        string            `json:"user,omitempty"`

	FrequencyPenalty *float64       `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64       `json:"presence_penalty,omitempty"`
	N                *int           `json:"n,omitempty"`
	Seed             *int64         `json:"seed,omitempty"`
	LogitBias        map[string]int `json:"logit_bias,omitempty"`
	Logprobs         *bool          `json:"logprobs,omitempty"`
	TopLogprobs      *int           `json:"top_logprobs,omitempty"`
	MaxTokens        *int           `json:"max_tokens,omitempty"`            // deprecated but widely used
	MaxCompletion    *int           `json:"max_completion_tokens,omitempty"` // includes reasoning tokens
	Stop             json.RawMessage `json:"stop,omitempty"`                  // string | []string
	Stream           *bool           `json:"stream,omitempty"`
	StreamOptions    *StreamOptions  `json:"stream_options,omitempty"`
	ServiceTier      string          `json:"service_tier,omitempty"`
	ReasoningEffort  string          `json:"reasoning_effort,omitempty"`
	Store            *bool           `json:"store,omitempty"`
	ResponseFormat   *ResponseFormat `json:"response_format,omitempty"`

	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`        // "none" | "auto" | "required" | object
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
}

// ChatMessage is a single message in the conversation (request side).
// Content is polymorphic: string for plain text, array of ContentPart for
// multimodal — kept as RawMessage so variants pass through untouched.
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	Refusal    string          `json:"refusal,omitempty"`
}

// ContentPart is one element of a multimodal content array.
// Type discriminates: "text" | "image_url" | "input_audio" | "file".
type ContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL *ImageURL       `json:"image_url,omitempty"`
	Audio    *InputAudio     `json:"input_audio,omitempty"`
	File     json.RawMessage `json:"file,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type ResponseFormat struct {
	Type       string          `json:"type"`
	JSONSchema json.RawMessage `json:"json_schema,omitempty"`
}

type Tool struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func (r *FullChatRequest) Validate() error {
	if r.Model == "" {
		return errors.New("model is required")
	}
	if len(r.Messages) == 0 {
		return errors.New("messages must contain at least one entry")
	}
	for i, m := range r.Messages {
		if m.Role == "" {
			return fmt.Errorf("messages[%d].role is required", i)
		}
	}
	return nil
}

// --- Response types ---

type ChatResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
	ServiceTier       string       `json:"service_tier,omitempty"`
	Choices           []Choice     `json:"choices"`
	Usage             *Usage       `json:"usage,omitempty"`
}

type ChatStreamChunk struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	Created           int64           `json:"created"`
	Model             string          `json:"model"`
	SystemFingerprint string          `json:"system_fingerprint,omitempty"`
	ServiceTier       string          `json:"service_tier,omitempty"`
	Choices           []StreamChoice  `json:"choices"`
	Usage             *Usage          `json:"usage,omitempty"`
}

type Choice struct {
	Index        int                  `json:"index"`
	Message      ChatResponseMessage  `json:"message"`
	FinishReason string               `json:"finish_reason"`
	Logprobs     *ChoiceLogprobs      `json:"logprobs,omitempty"`
}

// ChatResponseMessage is the assistant message in a non-streaming response.
// Content is nullable per the spec (null when refusal is set).
type ChatResponseMessage struct {
	Role        string          `json:"role"`
	Content     *string         `json:"content"`
	Refusal     *string         `json:"refusal,omitempty"`
	ToolCalls   []ToolCall      `json:"tool_calls,omitempty"`
	Annotations []Annotation    `json:"annotations,omitempty"`
}

// Annotation is a URL citation attached to an assistant message (web search tool).
type Annotation struct {
	Type       string      `json:"type"`
	URLCitation URLCitation `json:"url_citation"`
}

type URLCitation struct {
	StartIndex int    `json:"start_index"`
	EndIndex   int    `json:"end_index"`
	URL        string `json:"url"`
	Title      string `json:"title"`
}

type StreamChoice struct {
	Index        int             `json:"index"`
	Delta        StreamDelta     `json:"delta"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     *ChoiceLogprobs `json:"logprobs,omitempty"`
}

// StreamDelta is the incremental message fragment in a streaming chunk.
// Content is nullable (null on the final chunk).
type StreamDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   *string         `json:"content,omitempty"`
	Refusal   *string         `json:"refusal,omitempty"`
	ToolCalls []ToolCallChunk `json:"tool_calls,omitempty"`
}

// ToolCallChunk is a partial tool call as delivered in a streaming chunk.
// Index identifies which tool call this fragment belongs to.
type ToolCallChunk struct {
	Index    int                   `json:"index"`
	ID       string                `json:"id,omitempty"`
	Type     string                `json:"type,omitempty"`
	Function *ToolCallFunctionChunk `json:"function,omitempty"`
}

type ToolCallFunctionChunk struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ChoiceLogprobs struct {
	Content []TokenLogprob `json:"content"`
	Refusal []TokenLogprob `json:"refusal"`
}

type TokenLogprob struct {
	Token       string         `json:"token"`
	Logprob     float64        `json:"logprob"`
	Bytes       []int          `json:"bytes,omitempty"`
	TopLogprobs []TopLogprob   `json:"top_logprobs,omitempty"`
}

type TopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []int   `json:"bytes,omitempty"`
}

type Usage struct {
	PromptTokens      int                     `json:"prompt_tokens"`
	CompletionTokens  int                     `json:"completion_tokens"`
	TotalTokens       int                     `json:"total_tokens"`
	PromptDetails     *PromptTokenDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *CompletionTokenDetails `json:"completion_tokens_details,omitempty"`
}

type PromptTokenDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
	AudioTokens  int `json:"audio_tokens,omitempty"`
}

type CompletionTokenDetails struct {
	ReasoningTokens          int `json:"reasoning_tokens,omitempty"`
	AudioTokens              int `json:"audio_tokens,omitempty"`
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens,omitempty"`
}
