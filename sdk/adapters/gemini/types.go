// Package gemini implements v1.Translator for the Google Gemini
// generateContent / streamGenerateContent wire shape (v1beta).
//
// Scope: request serialization, response parsing, streaming (both directions),
// and token extraction for the native Gemini REST API.
//
// Out of scope: Vertex AI regional endpoints (same wire, different URL/auth),
// multimodal Live API, embeddings (byte-pass via separate Spec), batch
// prediction — those are additive.
package gemini

import "encoding/json"

// geminiRequest is the POST body for generateContent / streamGenerateContent.
type geminiRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
	Tools             []geminiTool      `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is the union of all Gemini part variants. Only the populated
// fields are marshalled (omitempty on every variant field).
type geminiPart struct {
	// text / thought
	Text    string `json:"text,omitempty"`
	Thought bool   `json:"thought,omitempty"`

	// thoughtSignature is a base64 string attached by Gemini thinking models to
	// thought and functionCall parts. It must be echoed verbatim in the next
	// request's corresponding part for coherent multi-turn thinking. It rides on
	// the part object itself (not on the nested functionCall sub-object) per the
	// Gemini REST API shape.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`

	// inlineData (base64 image/audio)
	InlineData *inlineData `json:"inlineData,omitempty"`

	// fileData (GCS / File API URI)
	FileData *fileData `json:"fileData,omitempty"`

	// functionCall output from model
	FunctionCall *geminiFC `json:"functionCall,omitempty"`

	// functionResponse from user
	FunctionResponse *geminiFR `json:"functionResponse,omitempty"`
}

type inlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"` // base64-encoded
}

type fileData struct {
	MIMEType string `json:"mimeType,omitempty"`
	FileURI  string `json:"fileUri"`
}

type geminiFC struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"` // JSON object on the wire
}

type geminiFR struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"` // JSON object on the wire
}

type generationConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	TopK             *int            `json:"topK,omitempty"`
	MaxOutputTokens  *int            `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
	FrequencyPenalty *float64        `json:"frequencyPenalty,omitempty"`
	PresencePenalty  *float64        `json:"presencePenalty,omitempty"`
	CandidateCount   *int            `json:"candidateCount,omitempty"`
	ResponseMIMEType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   interface{}     `json:"responseSchema,omitempty"`
	ThinkingConfig   *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	ThinkingBudget  int  `json:"thinkingBudget,omitempty"`
	IncludeThoughts bool `json:"includeThoughts,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations,omitempty"`
}

type functionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolConfig struct {
	FunctionCallingConfig *functionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type functionCallingConfig struct {
	Mode                 string   `json:"mode,omitempty"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// geminiResponse is the body returned by generateContent.
type geminiResponse struct {
	Candidates    []candidate    `json:"candidates"`
	UsageMetadata *usageMetadata `json:"usageMetadata,omitempty"`
	ModelVersion  string         `json:"modelVersion,omitempty"`
}

type candidate struct {
	Content      *geminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        int            `json:"index"`
}

type usageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount"`
	CandidatesTokenCount    int `json:"candidatesTokenCount"`
	TotalTokenCount         int `json:"totalTokenCount"`
	CachedContentTokenCount int `json:"cachedContentTokenCount"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount"`
}
