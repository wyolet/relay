package v1

import (
	"encoding/json"
	"testing"
)

// --- Model field: string vs array ---

func TestParseModelString(t *testing.T) {
	body := `{"model":"x","input":"hi"}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Model) != 1 || req.Model[0] != "x" {
		t.Errorf("model: %v", req.Model)
	}
}

func TestParseModelSingleArray(t *testing.T) {
	body := `{"model":["x"],"input":"hi"}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Model) != 1 || req.Model[0] != "x" {
		t.Errorf("model: %v", req.Model)
	}
}

func TestParseModelArrayMultiplex(t *testing.T) {
	body := `{"model":["x","y"],"input":"hi"}`
	_, err := Parse([]byte(body))
	if err != ErrMultiplexNotImplemented {
		t.Errorf("expected ErrMultiplexNotImplemented, got %v", err)
	}
}

func TestParseModelMissing(t *testing.T) {
	body := `{"input":"hi"}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Error("expected error when model is missing")
	}
}

func TestParseModelEmptyString(t *testing.T) {
	body := `{"model":"","input":"hi"}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Error("expected error for empty model string")
	}
}

func TestParseModelEmptyInArray(t *testing.T) {
	body := `{"model":[""],"input":"hi"}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Error("expected error for empty model name in array")
	}
}

// --- ModelConfig ---

func TestParseModelConfigKeyNotInModel(t *testing.T) {
	body := `{"model":"x","input":"hi","model_config":{"y":{}}}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Error("expected error for model_config key not in model list")
	}
}

func TestParseModelConfigMatchingKey(t *testing.T) {
	body := `{"model":"x","input":"hi","model_config":{"x":{"sampling":{"temperature":0.5}}}}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["x"]
	if opts == nil {
		t.Fatal("expected model_config[x]")
	}
	if opts.Sampling == nil {
		t.Fatal("expected sampling")
	}
	if opts.Sampling.Temperature == nil || *opts.Sampling.Temperature != 0.5 {
		t.Errorf("temperature: %v", opts.Sampling.Temperature)
	}
}

func TestParseCacheConfig(t *testing.T) {
	body := `{"model":"x","cache_config":{"instructions":true,"tools":true},"input":[` +
		`{"type":"message","role":"user","content":"stable","cache_config":{"anchor":true}},` +
		`{"type":"message","role":"user","content":"latest"}]}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.CacheConfig == nil || !req.CacheConfig.Instructions || !req.CacheConfig.Tools {
		t.Fatalf("request cache_config: %+v", req.CacheConfig)
	}
	anchored, ok := req.Input[0].(*Message)
	if !ok || anchored.CacheConfig == nil || !anchored.CacheConfig.Anchor {
		t.Errorf("item[0] cache_config anchor: %+v", req.Input[0])
	}
	if m, ok := req.Input[1].(*Message); !ok || m.CacheConfig != nil {
		t.Errorf("item[1] should have no cache_config: %+v", req.Input[1])
	}
}

func TestParseModelConfigNullValue(t *testing.T) {
	body := `{"model":"x","input":"hi","model_config":{"x":null}}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	// null config entry is allowed; value is nil (use vendor defaults)
	if opts, ok := req.ModelConfig["x"]; !ok || opts != nil {
		t.Errorf("expected nil model_config[x], got %v (present=%v)", opts, ok)
	}
}

func TestParseModelConfigEmptyObject(t *testing.T) {
	body := `{"model":"x","input":"hi","model_config":{"x":{}}}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["x"]
	if opts == nil {
		t.Fatal("expected non-nil model_config[x] for empty object")
	}
	if opts.Sampling != nil || opts.Tools != nil || opts.Reasoning != nil || opts.Output != nil {
		t.Errorf("expected all fields nil for empty object opts: %+v", opts)
	}
}

// --- OutputMode ---

func TestParseOutputModeStream(t *testing.T) {
	body := `{"model":"x","input":"hi","output_mode":"stream"}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.OutputMode != OutputModeStream {
		t.Errorf("output_mode: %q", req.OutputMode)
	}
}

func TestParseOutputModeOmitted(t *testing.T) {
	body := `{"model":"x","input":"hi"}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.OutputMode != "" {
		t.Errorf("expected empty output_mode, got %q", req.OutputMode)
	}
}

// --- String input normalization ---

func TestParseStringInput(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hello"}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Model) != 1 || req.Model[0] != "gpt-4o" {
		t.Errorf("model: %v", req.Model)
	}
	if len(req.Input) != 1 {
		t.Fatalf("expected 1 item, got %d", len(req.Input))
	}
	msg, ok := req.Input[0].(*Message)
	if !ok {
		t.Fatalf("expected *Message, got %T", req.Input[0])
	}
	if msg.Role != RoleUser {
		t.Errorf("role: %q", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 part, got %d", len(msg.Content))
	}
	tp, ok := msg.Content[0].(*TextPart)
	if !ok {
		t.Fatalf("expected *TextPart, got %T", msg.Content[0])
	}
	if tp.Text != "hello" {
		t.Errorf("text: %q", tp.Text)
	}
}

func TestParseArrayInput(t *testing.T) {
	body := `{"model":"gpt-4o","input":[{"type":"message","role":"user","content":"hi"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Input) != 2 {
		t.Fatalf("expected 2 items, got %d", len(req.Input))
	}
}

func TestParseRequiresInput(t *testing.T) {
	body := `{"model":"gpt-4o"}`
	_, err := Parse([]byte(body))
	if err == nil {
		t.Error("expected error when input is missing")
	}
}

// --- Tools (now in ModelConfig) ---

func TestParseWithTools(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": "what is the weather?",
		"model_config": {
			"gpt-4o": {
				"tools": {
					"definitions": [{"type":"function","name":"get_weather","parameters":{"type":"object"}}],
					"choice": "auto"
				}
			}
		}
	}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-4o"]
	if opts == nil || opts.Tools == nil {
		t.Fatal("expected tools config")
	}
	if len(opts.Tools.Definitions) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(opts.Tools.Definitions))
	}
	if opts.Tools.Definitions[0].ToolType() != ToolTypeFunction {
		t.Errorf("tool type: %v", opts.Tools.Definitions[0].ToolType())
	}
	if opts.Tools.Choice == nil {
		t.Fatal("expected tool choice")
	}
	if opts.Tools.Choice.Mode != "auto" {
		t.Errorf("tool choice mode: %q", opts.Tools.Choice.Mode)
	}
}

func TestParseWithServerAndMCPToolsInModelConfig(t *testing.T) {
	body := `{
		"model": "gpt-4o",
		"input": "search for cats",
		"model_config": {
			"gpt-4o": {
				"tools": {
					"definitions": [
						{"type":"server","name":"web_search"},
						{"type":"mcp","server_url":"https://mcp.example.com"}
					]
				}
			}
		}
	}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["gpt-4o"]
	if opts == nil || opts.Tools == nil {
		t.Fatal("expected tools config")
	}
	defs := opts.Tools.Definitions
	if len(defs) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(defs))
	}
	if defs[0].ToolType() != ToolTypeServer {
		t.Errorf("tool[0] type: %v", defs[0].ToolType())
	}
	if defs[1].ToolType() != ToolTypeMCP {
		t.Errorf("tool[1] type: %v", defs[1].ToolType())
	}
}

// --- Extensions ---

func TestParseWithExtensions(t *testing.T) {
	body := `{"model":"gpt-4o","input":"hi","extensions":{"cache_control":{"type":"ephemeral"}}}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Extensions) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(req.Extensions))
	}
	if _, ok := req.Extensions["cache_control"]; !ok {
		t.Error("expected cache_control extension")
	}
}

// --- Reasoning (now in ModelConfig) ---

func TestParseReasoningConfig(t *testing.T) {
	body := `{"model":"o3","input":"think hard","model_config":{"o3":{"reasoning":{"effort":"high"}}}}`
	req, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	opts := req.ModelConfig["o3"]
	if opts == nil || opts.Reasoning == nil {
		t.Fatal("expected reasoning config")
	}
	if opts.Reasoning.Effort != "high" {
		t.Errorf("effort: %q", opts.Reasoning.Effort)
	}
}

// --- Invalid JSON ---

func TestParseInvalidJSON(t *testing.T) {
	_, err := Parse([]byte(`{not json}`))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

// --- Round-trip ---

func TestParseRoundTrip(t *testing.T) {
	temp := 0.7
	maxTok := 512
	req := &Request{
		Model: ModelRefs{"gpt-4o"},
		Input: []Item{
			&Message{
				Role:    RoleUser,
				Content: []Part{&TextPart{Text: "hi"}},
			},
		},
		Instructions: "be concise",
		ModelConfig: map[string]*ModelOpts{
			"gpt-4o": {
				Sampling: &SamplingParams{
					Temperature: &temp,
					MaxTokens:   &maxTok,
				},
			},
		},
		OutputMode: OutputModeSync,
		User:       "u1",
		Metadata:   map[string]string{"k": "v"},
		Extensions: map[string]json.RawMessage{
			"cache_control": json.RawMessage(`{"type":"ephemeral"}`),
		},
	}

	// Marshal to wire (exclude Input since it's json:"-")
	type wireReq struct {
		Model        ModelRefs                  `json:"model"`
		Instructions string                     `json:"instructions,omitempty"`
		ModelConfig  map[string]*ModelOpts      `json:"model_config,omitempty"`
		OutputMode   string                     `json:"output_mode,omitempty"`
		User         string                     `json:"user,omitempty"`
		Metadata     map[string]string          `json:"metadata,omitempty"`
		Extensions   map[string]json.RawMessage `json:"extensions,omitempty"`
		Input        string                     `json:"input"`
	}
	wire := wireReq{
		Model:        req.Model,
		Instructions: req.Instructions,
		ModelConfig:  req.ModelConfig,
		OutputMode:   req.OutputMode,
		User:         req.User,
		Metadata:     req.Metadata,
		Extensions:   req.Extensions,
		Input:        "hi",
	}
	b, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}

	req2, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}

	if len(req2.Model) != 1 || req2.Model[0] != "gpt-4o" {
		t.Errorf("model: %v", req2.Model)
	}
	if req2.Instructions != "be concise" {
		t.Errorf("instructions: %q", req2.Instructions)
	}
	if req2.OutputMode != OutputModeSync {
		t.Errorf("output_mode: %q", req2.OutputMode)
	}
	if req2.User != "u1" {
		t.Errorf("user: %q", req2.User)
	}
	if req2.Metadata["k"] != "v" {
		t.Errorf("metadata: %v", req2.Metadata)
	}
	opts := req2.ModelConfig["gpt-4o"]
	if opts == nil || opts.Sampling == nil {
		t.Fatal("expected sampling in round-tripped request")
	}
	if opts.Sampling.Temperature == nil || *opts.Sampling.Temperature != 0.7 {
		t.Errorf("temperature: %v", opts.Sampling.Temperature)
	}
	if opts.Sampling.MaxTokens == nil || *opts.Sampling.MaxTokens != 512 {
		t.Errorf("max_tokens: %v", opts.Sampling.MaxTokens)
	}
	if len(req2.Extensions) != 1 {
		t.Fatalf("expected 1 extension, got %d", len(req2.Extensions))
	}
}
