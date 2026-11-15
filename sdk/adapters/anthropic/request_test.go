package anthropic

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	v1 "github.com/wyolet/relay/sdk/v1"
)

type direction int

const (
	both direction = iota
	parseOnly
	serializeOnly
)

// body is a Messages request holding the test model, max_tokens 4096 and members.
func body(members string) string {
	return `{"model":"` + testModel + `","max_tokens":4096,` + members + `}`
}

// request is the canonical form of body(""), edited.
func request(edit func(r *v1.Request, opts *v1.ModelOpts)) *v1.Request {
	opts := &v1.ModelOpts{Sampling: &v1.SamplingParams{MaxTokens: ptr(4096)}}
	r := &v1.Request{
		Model:       v1.ModelRefs{testModel},
		OutputMode:  v1.OutputModeSync,
		ModelConfig: map[string]*v1.ModelOpts{testModel: opts},
	}
	edit(r, opts)
	return r
}

func input(items ...v1.Item) func(*v1.Request, *v1.ModelOpts) {
	return func(r *v1.Request, _ *v1.ModelOpts) { r.Input = items }
}

const (
	hiTurn   = `{"role":"user","content":"hi"}`
	hiOnly   = `"messages":[` + hiTurn + `]`
	ccBlock  = `"cache_control":{"type":"ephemeral"}`
	cc1h     = `"cache_control":{"type":"ephemeral","ttl":"1h"}`
	pngBlock = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QQ=="}}`
	pdfBlock = `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}`

	anchoredTurn = `{"role":"user","content":[{"type":"text","text":"q",` + ccBlock + `}]}`
)

var (
	pngPart = &v1.ImagePart{ImageURL: "data:image/png;base64,QQ=="}
	pdfPart = &v1.FilePart{FileData: "JVBERi0=", MediaType: "application/pdf"}
)

func markerTurn(text string) string {
	return `{"role":"user","content":` + string(mustJSON(v1.WrapSystemMarker(text))) + `}`
}

func signedThinking(text, sig string) *v1.Reasoning {
	return &v1.Reasoning{
		Content:      text,
		Summary:      []v1.SummaryText{{Text: text}},
		ProviderData: json.RawMessage(`{"signature":"` + sig + `","thinking":"` + text + `","type":"thinking"}`),
	}
}

var requestCases = []struct {
	name string
	dir  direction
	wire string
	req  *v1.Request
}{
	{
		name: "text turns",
		wire: body(`"messages":[` + hiTurn + `,{"role":"assistant","content":"yo"}]`),
		req:  request(input(userText("hi"), assistantText("yo"))),
	},
	{
		name: "system, sampling, stream and user",
		wire: body(`"system":"Be terse.","temperature":0.5,"top_p":0.9,"top_k":40,"stop_sequences":["END","STOP"],"stream":true,"metadata":{"user_id":"u1"},` + hiOnly),
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			r.Instructions, r.OutputMode, r.User = "Be terse.", v1.OutputModeStream, "u1"
			opts.Sampling = &v1.SamplingParams{Temperature: ptr(0.5), TopP: ptr(0.9), TopK: ptr(40), MaxTokens: ptr(4096), Stop: []string{"END", "STOP"}}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "system blocks join their non-empty text",
		dir:  parseOnly,
		wire: body(`"system":[{"type":"text","text":"a"},{"type":"image"},{"type":"thinking","text":"no"},{"type":"text","text":""},{"type":"text","text":"b"}],` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Instructions, r.Input = "a\nb", []v1.Item{userText("hi")}
		}),
	},
	{
		name: "max_tokens falls back when sampling is unset",
		dir:  serializeOnly,
		wire: body(hiOnly),
		req:  &v1.Request{Model: v1.ModelRefs{testModel}, ModelConfig: map[string]*v1.ModelOpts{testModel: nil}, Input: []v1.Item{userText("hi")}},
	},
	{
		name: "function tool with forced choice",
		wire: body(`"tools":[{"name":"f","description":"d","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"f","disable_parallel_tool_use":true},` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Tools = &v1.ToolsConfig{
				Definitions: v1.Tools{&v1.FunctionTool{Name: "f", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)}},
				Choice:      &v1.ToolChoice{Mode: "function", FunctionName: "f"},
				Parallel:    ptr(false),
			}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "tool without schema and server tool",
		dir:  parseOnly,
		wire: body(`"tools":[{"name":"g"},{"type":"web_search_20250305","name":"web_search"}],` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{
				&v1.FunctionTool{Name: "g", Parameters: json.RawMessage(`{}`)},
				&v1.ServerTool{Name: "web_search"},
			}}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "nil tool parameters become an empty schema",
		dir:  serializeOnly,
		wire: body(`"tools":[{"name":"g","input_schema":{}}],` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{&v1.FunctionTool{Name: "g"}}}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		// tool_choice has no allowed list: only the allowed tools are sent.
		name: "allowed tools under a required choice",
		dir:  serializeOnly,
		wire: body(`"tools":[{"name":"f","input_schema":{}},{"name":"h","input_schema":{}}],"tool_choice":{"type":"any"},` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Tools = &v1.ToolsConfig{
				Definitions: v1.Tools{&v1.FunctionTool{Name: "f"}, &v1.FunctionTool{Name: "g"}, &v1.FunctionTool{Name: "h"}},
				Choice:      &v1.ToolChoice{Mode: "required", Allowed: []string{"h", "f"}},
			}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "allowed tools under an auto choice",
		dir:  serializeOnly,
		wire: body(`"tools":[{"name":"g","input_schema":{}}],"tool_choice":{"type":"auto"},` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Tools = &v1.ToolsConfig{
				Definitions: v1.Tools{&v1.FunctionTool{Name: "f"}, &v1.FunctionTool{Name: "g"}},
				Choice:      &v1.ToolChoice{Mode: "auto", Allowed: []string{"g"}},
			}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "parallel off without a choice rides an auto choice",
		dir:  serializeOnly,
		wire: body(`"tools":[{"name":"g","input_schema":{}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true},` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{&v1.FunctionTool{Name: "g"}}, Parallel: ptr(false)}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "parallel off without tools writes no tool_choice",
		dir:  serializeOnly,
		wire: body(hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Tools = &v1.ToolsConfig{Parallel: ptr(false)}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "streaming tools ask for eager input",
		wire: body(`"stream":true,"tools":[{"name":"f","input_schema":{},"eager_input_streaming":true}],` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.OutputMode = v1.OutputModeStream
			r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{&v1.FunctionTool{Name: "f", Parameters: json.RawMessage(`{}`)}}}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "manual thinking budget",
		wire: `{"model":"` + testModel + `","max_tokens":8000,"thinking":{"type":"enabled","budget_tokens":2000},` + hiOnly + `}`,
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Sampling.MaxTokens = ptr(8000)
			opts.Reasoning = &v1.ReasoningConfig{BudgetTokens: ptr(2000)}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "budget is floored and max_tokens leaves room for the answer",
		dir:  serializeOnly,
		wire: `{"model":"` + testModel + `","max_tokens":5120,"thinking":{"type":"enabled","budget_tokens":1024},` + hiOnly + `}`,
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Sampling.MaxTokens = ptr(1024)
			opts.Reasoning = &v1.ReasoningConfig{BudgetTokens: ptr(500)}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "adaptive thinking with a summary",
		wire: body(`"thinking":{"type":"adaptive","display":"summarized"},` + hiOnly),
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Reasoning = &v1.ReasoningConfig{Summary: "auto"}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		// minimal is not an Anthropic level; it passes through for the upstream to judge.
		name: "effort rides output_config verbatim",
		wire: body(`"thinking":{"type":"adaptive"},"output_config":{"effort":"minimal"},` + hiOnly),
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Reasoning = &v1.ReasoningConfig{Effort: "minimal"}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "effort with a manual budget",
		wire: `{"model":"` + testModel + `","max_tokens":8000,"thinking":{"type":"enabled","budget_tokens":3000},"output_config":{"effort":"medium"},` + hiOnly + `}`,
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Sampling.MaxTokens = ptr(8000)
			opts.Reasoning = &v1.ReasoningConfig{Effort: "medium", BudgetTokens: ptr(3000)}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "effort without thinking",
		dir:  parseOnly,
		wire: body(`"output_config":{"effort":"low"},` + hiOnly),
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Reasoning = &v1.ReasoningConfig{Effort: "low"}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "disabled thinking opens no reasoning config",
		dir:  parseOnly,
		wire: body(`"thinking":{"type":"disabled"},` + hiOnly),
		req:  request(input(userText("hi"))),
	},
	{
		name: "thinking drops temperature, top_p and top_k",
		dir:  serializeOnly,
		wire: body(`"thinking":{"type":"adaptive"},"stop_sequences":["END"],` + hiOnly),
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Sampling = &v1.SamplingParams{Temperature: ptr(0.7), TopP: ptr(0.9), TopK: ptr(5), Stop: []string{"END"}}
			opts.Reasoning = &v1.ReasoningConfig{BudgetTokens: ptr(0)}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "a negative budget asks for adaptive thinking",
		dir:  serializeOnly,
		wire: body(`"thinking":{"type":"adaptive"},` + hiOnly),
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Reasoning = &v1.ReasoningConfig{BudgetTokens: ptr(-1)}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "json schema format shares output_config with effort",
		wire: body(`"thinking":{"type":"adaptive"},"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object","additionalProperties":false}}},` + hiOnly),
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Reasoning = &v1.ReasoningConfig{Effort: "high"}
			opts.Output = &v1.OutputConfig{Format: &v1.Format{Type: "json_schema", Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "json schema format alone adds no tools",
		wire: body(`"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}},` + hiOnly),
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Output = &v1.OutputConfig{Format: &v1.Format{Type: "json_schema", Schema: json.RawMessage(`{"type":"object"}`)}}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "text format writes no output_config",
		dir:  serializeOnly,
		wire: body(hiOnly),
		req: request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Output = &v1.OutputConfig{Format: &v1.Format{Type: "text"}}
			r.Input = []v1.Item{userText("hi")}
		}),
	},
	{
		name: "user media blocks",
		wire: body(`"messages":[{"role":"user","content":[{"type":"text","text":"look"},` + pngBlock + `,{"type":"image","source":{"type":"url","url":"https://x/a;b,c.png"}},` + pdfBlock + `,{"type":"document","source":{"type":"url","url":"https://x/a.pdf"},` + ccBlock + `}]}]`),
		req: request(input(anchored(&v1.Message{Role: v1.RoleUser, Content: []v1.Part{
			&v1.TextPart{Text: "look"}, pngPart, &v1.ImagePart{ImageURL: "https://x/a;b,c.png"}, pdfPart, &v1.FilePart{FileURL: "https://x/a.pdf"},
		}}))),
	},
	{
		name: "image without media type and document by file id",
		dir:  parseOnly,
		wire: body(`"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"QQ=="}},{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]`),
		req: request(input(&v1.Message{Role: v1.RoleUser, Content: []v1.Part{
			&v1.ImagePart{ImageURL: "data:application/octet-stream;base64,QQ=="}, &v1.FilePart{FileID: "file_1"},
		}})),
	},
	{
		name: "file data without media type is sent as pdf",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"user","content":[{"type":"text","text":"see"},` + pdfBlock + `]}]`),
		req:  request(input(&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.OutputTextPart{Text: "see"}, &v1.FilePart{FileData: "JVBERi0="}}})),
	},
	{
		name: "tool loop",
		wire: body(`"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"t1","name":"f","input":{"a":1}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]`),
		req: request(input(
			userText("go"), assistantText("calling"),
			&v1.FunctionCall{ID: "t1", CallID: "t1", Name: "f", Arguments: `{"a":1}`},
			&v1.FunctionCallOutput{CallID: "t1", Output: "ok"},
		)),
	},
	{
		name: "tool_use without input, tool_result of text blocks beside user text",
		dir:  parseOnly,
		wire: body(`"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},{"type":"text","text":"and"}]}]`),
		req: request(input(
			&v1.FunctionCall{ID: "t1", CallID: "t1", Name: "f", Arguments: `{}`},
			&v1.FunctionCallOutput{CallID: "t1", Output: "ab"},
			userText("and"),
		)),
	},
	{
		name: "empty and unparseable call arguments",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{}},{"type":"tool_use","id":"t2","name":"g","input":{"_raw":"{\"a\":"}}]}]`),
		req: request(input(
			anchored(assistantText("")),
			&v1.FunctionCall{CallID: "t1", Name: "f"},
			&v1.FunctionCall{CallID: "t2", Name: "g", Arguments: `{"a":`},
		)),
	},
	{
		name: "tool result carrying an image and a document",
		wire: body(`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"here"},` + pngBlock + `,` + pdfBlock + `]}]}]`),
		req: request(input(&v1.FunctionCallOutput{
			CallID: "t1", Output: "here", Content: []v1.Part{&v1.TextPart{Text: "here"}, pngPart, pdfPart},
		})),
	},
	{
		name: "media tool result falls back to output for its text",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[` + pngBlock + `,{"type":"text","text":"caption"}]},{"type":"tool_result","tool_use_id":"t2","content":[` + pngBlock + `]}]}]`),
		req: request(input(
			&v1.FunctionCallOutput{CallID: "t1", Output: "caption", Content: []v1.Part{&v1.TextPart{}, pngPart}},
			&v1.FunctionCallOutput{CallID: "t2", Content: []v1.Part{pngPart}},
		)),
	},
	{
		name: "text-only tool result content is joined",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ab"},{"type":"tool_result","tool_use_id":"t2","content":"out"}]}]`),
		req: request(input(
			&v1.FunctionCallOutput{CallID: "t1", Content: []v1.Part{&v1.TextPart{Text: "a"}, &v1.TextPart{Text: "b"}}},
			&v1.FunctionCallOutput{CallID: "t2", Output: "out", Content: []v1.Part{&v1.TextPart{Text: "parts"}}},
		)),
	},
	{
		// Anthropic rejects a tool_use turn that does not lead with its thinking block, so the run is one message.
		name: "signed thinking, text and tool call replay as one assistant message",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"SIG"},{"type":"text","text":"calling"},{"type":"tool_use","id":"t1","name":"f","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]`),
		req: request(input(
			userText("go"), signedThinking("plan", "SIG"), assistantText("calling"),
			&v1.FunctionCall{ID: "fc_2", CallID: "t1", Name: "f", Arguments: `{}`},
			&v1.FunctionCallOutput{CallID: "t1", Output: "ok"},
		)),
	},
	{
		name: "signed thinking with empty text keeps its signature",
		wire: body(`"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"SIG"},{"type":"tool_use","id":"t1","name":"f","input":{}}]}]`),
		req: request(input(
			signedThinking("", "SIG"),
			&v1.FunctionCall{ID: "t1", CallID: "t1", Name: "f", Arguments: `{}`},
		)),
	},
	{
		name: "redacted thinking replays its data in place",
		wire: body(`"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"SIG"},{"type":"redacted_thinking","data":"ENC"},{"type":"tool_use","id":"t1","name":"f","input":{}}]}]`),
		req: request(input(
			signedThinking("plan", "SIG"),
			&v1.Reasoning{ProviderData: json.RawMessage(`{"data":"ENC","type":"redacted_thinking"}`)},
			&v1.FunctionCall{ID: "t1", CallID: "t1", Name: "f", Arguments: `{}`},
		)),
	},
	{
		// An unsigned thinking block is rejected upstream, so another vendor's reasoning is left out.
		name: "unsigned reasoning is not replayed",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"user","content":"go"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]`),
		req: request(input(
			userText("go"), &v1.Reasoning{ID: "rs_0", Content: "other vendor"},
			&v1.Reasoning{ProviderData: json.RawMessage(`{"type":"reasoning","signature":"s"}`)},
			&v1.Reasoning{ProviderData: json.RawMessage(`{"type":"thought","signature":"s"}`)},
			&v1.Reasoning{ProviderData: json.RawMessage(`{"type":"thinking","thinking":"unsigned"}`)},
			assistantText("answer"), userText("next"),
		)),
	},
	{
		// A thinking-only run closes as its own message mid-history; at the end it would be a prefill, which Anthropic rejects.
		name: "thinking leads the assistant message its run closes",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"SIG"}]},` + markerTurn("rule") +
			`,{"role":"assistant","content":[{"type":"thinking","thinking":"p2","signature":"S2"},{"type":"text","text":"a"}]},{"role":"user","content":"u"}` +
			`,{"role":"assistant","content":[{"type":"text","text":"b"},{"type":"text","text":"c"}]},{"role":"user","content":"end"}]`),
		req: request(input(
			signedThinking("plan", "SIG"), systemText("rule"), signedThinking("p2", "S2"), assistantText("a"), userText("u"),
			assistantText("b"), assistantText("c"), userText("end"), signedThinking("tail", "S3"),
		)),
	},
	{
		name: "unsigned thinking carries no provider data",
		dir:  parseOnly,
		wire: body(`"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}]`),
		req:  request(input(&v1.Reasoning{Content: "hmm", Summary: []v1.SummaryText{{Text: "hmm"}}})),
	},
	{
		name: "content elements that are not objects are skipped",
		dir:  parseOnly,
		wire: body(`"messages":[{"role":"system","content":["s",{"type":"text","text":"x"}]},{"role":"user","content":[5,{"type":"text","text":"y"}]}]`),
		req:  request(input(systemText("x"), userText("y"))),
	},
	{
		name: "user turn drops a tool_use block and keeps a marker beside other content as text",
		dir:  parseOnly,
		wire: body(`"messages":[{"role":"user","content":[{"type":"tool_use","id":"u"},{"type":"text","text":"y"}]}` +
			`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"},{"type":"text","text":` + string(mustJSON(v1.WrapSystemMarker("r"))) + `}]}` +
			`,{"role":"user","content":[{"type":"text","text":` + string(mustJSON(v1.WrapSystemMarker("r"))) + `},{"type":"text","text":"more"}]}]`),
		req: request(input(
			userText("y"), &v1.FunctionCallOutput{CallID: "t1", Output: "ok"}, userText(v1.WrapSystemMarker("r")),
			&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.TextPart{Text: v1.WrapSystemMarker("r")}, &v1.TextPart{Text: "more"}}},
		)),
	},
	{
		name: "system items after the conversation starts become marker turns in place",
		wire: body(`"messages":[{"role":"user","content":"a"},` + markerTurn("r1") + `,{"role":"assistant","content":"b"},` + markerTurn("r2") + `,{"role":"user","content":"c"},` + markerTurn("r3") + `,` + markerTurn("r4") + `]`),
		req: request(input(
			userText("a"), systemText("r1"), assistantText("b"), systemText("r2"), userText("c"), systemText("r3"), systemText("r4"),
		)),
	},
	{
		name: "leading system and developer items join the instructions",
		dir:  serializeOnly,
		wire: body(`"system":"base\nlead\ndev",` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Instructions = "base"
			r.Input = []v1.Item{systemText(""), systemText("lead"), &v1.Message{Role: v1.RoleDeveloper, Content: []v1.Part{&v1.TextPart{Text: "dev"}}}, userText("hi")}
		}),
	},
	{
		name: "hoisted system item joins the instructions from any position",
		dir:  serializeOnly,
		wire: body(`"system":"late rule","messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"}]`),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Input = []v1.Item{userText("a"), assistantText("b"), &v1.Message{Role: v1.RoleSystem, Hoist: true, Content: []v1.Part{&v1.TextPart{Text: "late rule"}}}}
		}),
	},
	{
		// A tool_result must directly follow its tool_use turn.
		name: "system items inside an open tool round wait for its results",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"user","content":"run"},{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"run","input":{}},{"type":"tool_use","id":"c2","name":"run","input":{}}]}` +
			`,{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"d1"},{"type":"tool_result","tool_use_id":"c2","content":"d2"}]},` + markerTurn("m1") + `,` + markerTurn("m2") +
			`,{"role":"assistant","content":[{"type":"tool_use","id":"c3","name":"run","input":{}}]},` + markerTurn("m3") + `]`),
		req: request(input(
			userText("run"), &v1.FunctionCall{CallID: "c1", Name: "run", Arguments: `{}`}, &v1.FunctionCall{CallID: "c2", Name: "run", Arguments: `{}`},
			systemText("m1"), &v1.FunctionCallOutput{CallID: "c1", Output: "d1"}, systemText("m2"), &v1.FunctionCallOutput{CallID: "c2", Output: "d2"},
			&v1.FunctionCall{CallID: "c3", Name: "run", Arguments: `{}`}, systemText("m3"),
		)),
	},
	{
		name: "anchored system item keeps its breakpoint on the marker turn",
		wire: body(`"messages":[{"role":"user","content":"a"},{"role":"user","content":[{"type":"text","text":` + string(mustJSON(v1.WrapSystemMarker("rule"))) + `,` + ccBlock + `}]}]`),
		req:  request(input(userText("a"), anchored(systemText("rule")))),
	},
	{
		name: "system role inside messages stays positional",
		dir:  parseOnly,
		wire: body(`"messages":[{"role":"user","content":"a"},{"role":"system","content":"mid rule"},{"role":"assistant","content":"b"}]`),
		req:  request(input(userText("a"), systemText("mid rule"), assistantText("b"))),
	},
	{
		name: "quoted marker stays user text",
		dir:  parseOnly,
		wire: body(`"messages":[{"role":"user","content":` + string(mustJSON("quoting: "+v1.WrapSystemMarker("nice try")+" end")) + `}]`),
		req:  request(input(userText("quoting: " + v1.WrapSystemMarker("nice try") + " end"))),
	},
	{
		name: "unknown role reads as user",
		dir:  parseOnly,
		wire: body(`"messages":[{"role":"developer","content":"x"}]`),
		req:  request(input(userText("x"))),
	},
	{
		name: "instructions, tools and item anchors on the 1h tier",
		wire: body(`"system":[{"type":"text","text":"Be terse.",` + cc1h + `}],"tools":[{"name":"a","input_schema":{}},{"name":"b","input_schema":{},` + cc1h + `}],"messages":[{"role":"user","content":"q1"},{"role":"assistant","content":[{"type":"text","text":"a1",` + cc1h + `}]},{"role":"user","content":"q2"}]`),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Instructions = "Be terse."
			r.CacheConfig = &v1.CacheConfig{Instructions: true, Tools: true, TTL: "1h"}
			r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{
				&v1.FunctionTool{Name: "a", Parameters: json.RawMessage(`{}`)},
				&v1.FunctionTool{Name: "b", Parameters: json.RawMessage(`{}`)},
			}}
			r.Input = []v1.Item{userText("q1"), anchored(assistantText("a1")), userText("q2")}
		}),
	},
	{
		name: "tool call and result anchors on the default tier",
		wire: body(`"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"read","input":{},` + ccBlock + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok",` + ccBlock + `}]}]`),
		req: request(input(
			userText("go"),
			&v1.FunctionCall{ID: "c1", CallID: "c1", Name: "read", Arguments: `{}`, CacheConfig: anchor},
			&v1.FunctionCallOutput{CallID: "c1", Output: "ok", CacheConfig: anchor},
		)),
	},
	{
		name: "anchored assistant text beside a tool call",
		wire: body(`"messages":[{"role":"assistant","content":[{"type":"text","text":"a1",` + ccBlock + `},{"type":"tool_use","id":"c1","name":"read","input":{}}]}]`),
		req:  request(input(anchored(assistantText("a1")), &v1.FunctionCall{ID: "c1", CallID: "c1", Name: "read", Arguments: `{}`})),
	},
	{
		name: "5m breakpoint reads as the default tier",
		dir:  parseOnly,
		wire: body(`"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"5m"}}],` + hiOnly),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Instructions, r.CacheConfig, r.Input = "s", &v1.CacheConfig{Instructions: true}, []v1.Item{userText("hi")}
		}),
	},
	{
		name: "history marks a trailing user string",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"user","content":[{"type":"text","text":"hi",` + ccBlock + `}]}]`),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.CacheConfig, r.Input = &v1.CacheConfig{History: true}, []v1.Item{userText("hi")}
		}),
	},
	{
		name: "history marks a trailing tool call",
		dir:  serializeOnly,
		wire: body(`"messages":[{"role":"user","content":"go"},{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"read","input":{},` + ccBlock + `}]}]`),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.CacheConfig = &v1.CacheConfig{History: true}
			r.Input = []v1.Item{userText("go"), &v1.FunctionCall{CallID: "c1", Name: "read", Arguments: `{}`}}
		}),
	},
	{
		// Instructions, tools, history and three item anchors ask for six breakpoints; the two oldest item anchors go.
		name: "breakpoints beyond four drop the oldest item anchors",
		dir:  serializeOnly,
		wire: body(`"system":[{"type":"text","text":"Be terse.",` + ccBlock + `}],"tools":[{"name":"read","input_schema":{},` + ccBlock + `}],"messages":[{"role":"user","content":"q0"},` +
			`{"role":"user","content":[{"type":"text","text":"q1"}]},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"read","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r1",` + ccBlock + `}]},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"c2","name":"read","input":{}}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"r2",` + ccBlock + `}]}]`),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Instructions = "Be terse."
			r.CacheConfig = &v1.CacheConfig{Instructions: true, Tools: true, History: true}
			r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{&v1.FunctionTool{Name: "read", Parameters: json.RawMessage(`{}`)}}}
			r.Input = []v1.Item{
				userText("q0"), anchored(userText("q1")),
				&v1.FunctionCall{CallID: "c1", Name: "read", Arguments: `{}`, CacheConfig: anchor},
				&v1.FunctionCallOutput{CallID: "c1", Output: "r1", CacheConfig: anchor},
				&v1.FunctionCall{CallID: "c2", Name: "read", Arguments: `{}`},
				&v1.FunctionCallOutput{CallID: "c2", Output: "r2"},
			}
		}),
	},
	{
		name: "a tools breakpoint with no tools takes none of the four slots",
		dir:  serializeOnly,
		wire: body(`"messages":[` + anchoredTurn + `,` + anchoredTurn + `,` + anchoredTurn + `,` + anchoredTurn + `]`),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.CacheConfig = &v1.CacheConfig{Tools: true}
			r.Input = []v1.Item{anchored(userText("q")), anchored(userText("q")), anchored(userText("q")), anchored(userText("q"))}
		}),
	},
	{
		name: "unmarked tools take none of the four slots",
		wire: body(`"tools":[{"name":"f","input_schema":{}}],"messages":[` + anchoredTurn + `,` + anchoredTurn + `,` + anchoredTurn + `,` + anchoredTurn + `]`),
		req: request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{&v1.FunctionTool{Name: "f", Parameters: json.RawMessage(`{}`)}}}
			r.Input = []v1.Item{anchored(userText("q")), anchored(userText("q")), anchored(userText("q")), anchored(userText("q"))}
		}),
	},
}

func TestRequestMapping(t *testing.T) {
	for _, tc := range requestCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.dir != serializeOnly {
				for name, parse := range map[string]func([]byte) (*v1.Request, error){
					"ParseRequest": AnthropicTranslator{}.ParseRequest, "layered": parseRequestLayered,
				} {
					got, err := parse([]byte(tc.wire))
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if diff := cmp.Diff(tc.req, got); diff != "" {
						t.Errorf("%s (-want +got):\n%s", name, diff)
					}
				}
			}
			if tc.dir != parseOnly {
				got, err := AnthropicTranslator{}.SerializeRequest(tc.req)
				if err != nil {
					t.Fatal(err)
				}
				assertJSONEqual(t, got, tc.wire)
			}
		})
	}
}

func TestParseRequest_ModelConfigHoldsOnlyWhatTheBodySets(t *testing.T) {
	for members, want := range map[string]*v1.ModelOpts{
		`"stream":false`:         nil,
		`"temperature":0.5`:      {Sampling: &v1.SamplingParams{Temperature: ptr(0.5)}},
		`"top_p":0.9`:            {Sampling: &v1.SamplingParams{TopP: ptr(0.9)}},
		`"top_k":40`:             {Sampling: &v1.SamplingParams{TopK: ptr(40)}},
		`"stop_sequences":["x"]`: {Sampling: &v1.SamplingParams{Stop: []string{"x"}}},
		`"thinking":{"type":"enabled","budget_tokens":0}`:  {Reasoning: &v1.ReasoningConfig{}},
		`"thinking":{"type":"enabled","budget_tokens":-1}`: {Reasoning: &v1.ReasoningConfig{}},
	} {
		got, err := AnthropicTranslator{}.ParseRequest([]byte(`{"model":"m",` + members + `}`))
		if err != nil {
			t.Fatalf("%s: %v", members, err)
		}
		var config map[string]*v1.ModelOpts
		if want != nil {
			config = map[string]*v1.ModelOpts{"m": want}
		}
		if diff := cmp.Diff(config, got.ModelConfig); diff != "" {
			t.Errorf("%s (-want +got):\n%s", members, diff)
		}
	}
}

func TestSerializeRequest_RejectsUnknownFormatType(t *testing.T) {
	_, err := AnthropicTranslator{}.SerializeRequest(request(func(_ *v1.Request, opts *v1.ModelOpts) {
		opts.Output = &v1.OutputConfig{Format: &v1.Format{Type: "grammar"}}
	}))
	if !errors.Is(err, v1.ErrUnsupportedRequest) {
		t.Errorf("err = %v, want ErrUnsupportedRequest", err)
	}
}

func TestRequestKeys(t *testing.T) {
	if got := (AnthropicTranslator{}).RequestKeys(); got != (v1.RequestKeys{Stream: "stream"}) {
		t.Errorf("request keys = %+v", got)
	}
}

func TestToolChoiceMapping(t *testing.T) {
	for _, tc := range []struct {
		dir      direction
		wire     string
		choice   v1.ToolChoice
		parallel *bool
	}{
		{both, `{"type":"auto"}`, v1.ToolChoice{Mode: "auto"}, nil},
		{both, `{"type":"auto","disable_parallel_tool_use":true}`, v1.ToolChoice{Mode: "auto"}, ptr(false)},
		{both, `{"type":"any","disable_parallel_tool_use":true}`, v1.ToolChoice{Mode: "required"}, ptr(false)},
		{both, `{"type":"tool","name":"f"}`, v1.ToolChoice{Mode: "function", FunctionName: "f"}, nil},
		{both, `{"type":"none"}`, v1.ToolChoice{Mode: "none"}, nil},
		{parseOnly, `{"type":"tool","name":"f","disable_parallel_tool_use":false}`, v1.ToolChoice{Mode: "function", FunctionName: "f"}, ptr(true)},
		{parseOnly, `{"type":"future"}`, v1.ToolChoice{Mode: "future"}, nil},
		{serializeOnly, `{"type":"any"}`, v1.ToolChoice{Mode: "required"}, ptr(true)},
		{serializeOnly, `{"type":"none"}`, v1.ToolChoice{Mode: "none"}, ptr(false)},
		{serializeOnly, `{"type":"auto"}`, v1.ToolChoice{Mode: "future"}, nil},
	} {
		if tc.dir != serializeOnly {
			choice, parallel := anthropicParseToolChoice(json.RawMessage(tc.wire))
			if diff := cmp.Diff(&tc.choice, choice) + cmp.Diff(tc.parallel, parallel); diff != "" {
				t.Errorf("parse %s (-want +got):\n%s", tc.wire, diff)
			}
		}
		if tc.dir != parseOnly {
			assertJSONEqual(t, mustJSON(canonicalToolChoiceToAnthropic(&tc.choice, tc.parallel)), tc.wire)
		}
	}
}

func TestParseRequest_Rejects(t *testing.T) {
	for _, wire := range []string{
		`{"max_tokens":8,"messages":[]}`,
		`{"model":"m"`,
		`{"model":"m","messages":[{"role":"user","content":{}}]}`,
		`{"model":"m","messages":[{"role":"assistant","content":true}]}`,
		`{"model":"m","tools":["f"]}`,
		`{"model":"m","max_tokens":"x"}`,
		`{"model":"m","messages":[5]}`,
		`{"model":"m","messages":[{"role":"system","content":{}}]}`,
	} {
		if req, err := (AnthropicTranslator{}).ParseRequest([]byte(wire)); err == nil {
			t.Errorf("%s: parsed to %s, want an error", wire, mustJSON(req))
		}
	}
}

func TestSerializeRequest_Rejects(t *testing.T) {
	format := func(f v1.Format) *v1.Request {
		return request(func(r *v1.Request, opts *v1.ModelOpts) {
			opts.Output = &v1.OutputConfig{Format: &f}
			r.Input = []v1.Item{userText("hi")}
		})
	}
	for _, tc := range []struct {
		name        string
		req         *v1.Request
		unsupported bool
	}{
		{"no model", &v1.Request{Input: []v1.Item{userText("hi")}}, false},
		{"server tool", request(func(r *v1.Request, _ *v1.ModelOpts) {
			r.Tools = &v1.ToolsConfig{Definitions: v1.Tools{&v1.ServerTool{Name: "web_search"}}}
		}), false},
		{"file part by id", request(input(&v1.Message{Role: v1.RoleUser, Content: []v1.Part{&v1.FilePart{FileID: "file_1"}}})), false},
		{"file part by id in an assistant message", request(input(&v1.Message{Role: v1.RoleAssistant, Content: []v1.Part{&v1.FilePart{FileID: "file_1"}}})), false},
		{"part of an unknown type", request(input(&v1.Message{Role: v1.RoleUser, Content: []v1.Part{struct{ *v1.ImagePart }{}}})), false},
		{"file part by id in a tool result", request(input(&v1.FunctionCallOutput{CallID: "t1", Content: []v1.Part{&v1.FilePart{FileID: "file_1"}}})), false},
		{"json_object format", format(v1.Format{Type: "json_object"}), true},
		{"json_object format with a schema", format(v1.Format{Type: "json_object", Schema: json.RawMessage(`{"type":"object"}`)}), true},
		{"json_schema format without a schema", format(v1.Format{Type: "json_schema"}), true},
	} {
		out, err := AnthropicTranslator{}.SerializeRequest(tc.req)
		if err == nil {
			t.Errorf("%s: serialized to %s, want an error", tc.name, out)
		} else if errors.Is(err, v1.ErrUnsupportedRequest) != tc.unsupported {
			t.Errorf("%s: err = %v, ErrUnsupportedRequest = %v, want %v", tc.name, err, !tc.unsupported, tc.unsupported)
		}
	}
}

func TestCacheTTLTier(t *testing.T) {
	for ttl, want := range map[string]string{"": "", "1m": "", "5m": "", "garbage": "", "5m1s": "1h", "1h": "1h", "24h": "1h"} {
		if got := anthropicCacheTTL(&v1.CacheConfig{TTL: ttl}); got != want {
			t.Errorf("ttl %q → tier %q, want %q", ttl, got, want)
		}
	}
	if got := anthropicCacheTTL(nil); got != "" {
		t.Errorf("nil config → tier %q", got)
	}
}

// Thinking blocks cannot carry cache_control, so the history breakpoint lands on the block before them.
func TestMarkHistoryBreakpoint_SkipsThinking(t *testing.T) {
	markHistoryBreakpoint(nil, "1h")
	msgs := []anthropicCanonMsg{{Role: "assistant", Content: []map[string]any{
		{"type": "text", "text": "s"},
		{"type": "text", "text": "t"},
		{"type": "thinking", "thinking": "x", "signature": "s"},
		{"type": "redacted_thinking", "data": "d"},
	}}}
	markHistoryBreakpoint(msgs, "1h")
	assertJSONEqual(t, mustJSON(msgs[0].Content), `[{"type":"text","text":"s"},{"type":"text","text":"t",`+cc1h+`},{"type":"thinking","thinking":"x","signature":"s"},{"type":"redacted_thinking","data":"d"}]`)
}

func TestParseRequest_ThinkingPrecedesTheTextItFollowsOnTheWire(t *testing.T) {
	wire := body(`"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"plan","signature":"SIG"},{"type":"text","text":"calling"},{"type":"tool_use","id":"t1","name":"f","input":{}}]}]`)
	want := request(input(signedThinking("plan", "SIG"), assistantText("calling"), &v1.FunctionCall{ID: "t1", CallID: "t1", Name: "f", Arguments: `{}`}))
	got, err := AnthropicTranslator{}.ParseRequest([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}
