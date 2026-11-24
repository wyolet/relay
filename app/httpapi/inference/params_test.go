package inference

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/httpheader"
	v1 "github.com/wyolet/relay/sdk/v1"
)

var ccPaths = map[string]string{"temperature": "temperature", "top_p": "top_p"}

// openAIMaxTokens is the rename an OpenAI host declares for Chat Completions.
var openAIMaxTokens = map[adapters.Name]map[string]string{adapters.OpenAI: {"max_tokens": "max_completion_tokens"}}

func TestRenameBodyParams(t *testing.T) {
	spec := host.Spec{RequestParamRenames: openAIMaxTokens}
	for _, tc := range []struct {
		name, shape, body, want string
		superseded              []string
	}{
		{"absent", "openai", `{"model":"m","messages":[]}`, `{"model":"m","messages":[]}`, nil},
		{"present", "openai", `{"max_tokens":5, "messages":[]}`, `{"messages":[],"max_completion_tokens":5}`, nil},
		{"null value moves as sent", "openai", `{"max_tokens":null}`, `{"max_completion_tokens":null}`, nil},
		{"target already present wins", "openai", `{"max_tokens":5,"max_completion_tokens":9}`, `{"max_completion_tokens":9}`, []string{"max_tokens"}},
		{"nested key with the same name untouched", "openai", `{"metadata":{"max_tokens":5}}`, `{"metadata":{"max_tokens":5}}`, nil},
		{"key spelled in another case untouched", "openai", `{"Max_Tokens":5}`, `{"Max_Tokens":5}`, nil},
		{"adapter not in the table", "anthropic", `{"max_tokens":5}`, `{"max_tokens":5}`, nil},
		{"not an object", "openai", `[{"max_tokens":5}]`, `[{"max_tokens":5}]`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, superseded := editHopBody([]byte(tc.body), spec.ParamRenames(adapters.Name(tc.shape)), "")
			if string(got) != tc.want {
				t.Errorf("body = %s, want %s", got, tc.want)
			}
			if diff := cmp.Diff(tc.superseded, superseded); diff != "" {
				t.Errorf("superseded (-want +got):\n%s", diff)
			}
		})
	}
}

// Each hop gets its own host's form of one base body, which stays as built; a host without renames for the shape gets the base itself, at no cost.
func TestHopBody(t *testing.T) {
	cc := &binding.Binding{Spec: binding.Spec{Adapter: adapters.OpenAI}}
	hop := func(spec host.Spec) *plan.RouteHop {
		return &plan.RouteHop{Binding: cc, Host: &host.Host{Spec: spec}, ParamRenames: spec.ParamRenames(cc.Spec.Adapter)}
	}
	renaming := hop(host.Spec{RequestParamRenames: openAIMaxTokens})
	plain := hop(host.Spec{})
	otherShape := hop(host.Spec{RequestParamRenames: map[adapters.Name]map[string]string{adapters.OpenAIResponses: {"max_tokens": "x"}}})
	const base = `{"model":"m","max_tokens":5}`
	body := []byte(base)
	for _, tc := range []struct {
		name  string
		hop   *plan.RouteHop
		shape adapters.Name
		want  string
	}{
		{"renaming host", renaming, adapters.OpenAI, `{"model":"m","max_completion_tokens":5}`},
		{"plain host", plain, adapters.OpenAI, base},
		{"renaming host, other shape", renaming, adapters.OpenAIResponses, base},
		{"table for the shape sent, not the binding's", otherShape, adapters.OpenAIResponses, `{"model":"m","x":5}`},
	} {
		if got, _ := HopBody(tc.hop, tc.shape, body); string(got) != tc.want {
			t.Errorf("%s: body = %s, want %s", tc.name, got, tc.want)
		}
		if string(body) != base {
			t.Fatalf("%s: base body modified to %s", tc.name, body)
		}
	}
	if _, dropped := HopBody(renaming, adapters.OpenAI, []byte(`{"max_tokens":5,"max_completion_tokens":9}`)); !slices.Equal(dropped, []string{"max_tokens"}) {
		t.Errorf("dropped = %v, want [max_tokens]", dropped)
	}
	for _, h := range []*plan.RouteHop{plain, otherShape} {
		if n := testing.AllocsPerRun(100, func() { HopBody(h, adapters.OpenAI, body) }); n != 0 && !raceInstrumented {
			t.Errorf("HopBody for a host without renames for the shape allocates %v times", n)
		}
	}
}

// A hop sending another name than the body's gets it set in the body, after its host's renames; hops all sending the body's name cost nothing.
func TestHopBodyFor(t *testing.T) {
	cc := &binding.Binding{Spec: binding.Spec{Adapter: adapters.OpenAI}}
	renamer := host.Spec{RequestParamRenames: openAIMaxTokens}
	hops := []plan.RouteHop{
		{Binding: cc, Host: &host.Host{}, UpstreamModel: "m"},
		{Binding: cc, Host: &host.Host{Spec: renamer}, ParamRenames: renamer.ParamRenames(adapters.OpenAI), UpstreamModel: "acme/m-v1:0"},
	}
	const base = `{"model":"m","max_tokens":5}`
	body := []byte(base)
	hopBody := HopBodyFor(hops, "m")
	for i, want := range []string{base, `{"model":"acme/m-v1:0","max_completion_tokens":5}`} {
		if got, _ := hopBody(&hops[i], adapters.OpenAI, body); string(got) != want {
			t.Errorf("hop %d: body = %s, want %s", i, got, want)
		}
	}
	if got, _ := hopBody(&hops[1], adapters.Gemini, []byte(`{"contents":[]}`)); string(got) != `{"contents":[]}` {
		t.Errorf("body without a model member = %s, want it untouched", got)
	}
	if string(body) != base {
		t.Fatalf("base body modified to %s", body)
	}
	same := hops[:1]
	if n := testing.AllocsPerRun(100, func() { HopBodyFor(same, "m")(&same[0], adapters.OpenAI, body) }); n != 0 && !raceInstrumented {
		t.Errorf("HopBodyFor over hops sending the body's name allocates %v times", n)
	}
}

// A same-shape request to a host that renames a key reaches the upstream under the new name; a caller that also sent the new name keeps it and is told the old key went.
func TestDispatch_BytePass_RenamesParams(t *testing.T) {
	for name, tc := range map[string]struct {
		renames      map[adapters.Name]map[string]string
		caps         []model.Capabilities
		body         string
		wantUpstream string
		wantWarnings []string
	}{
		"renamed": {openAIMaxTokens, nil, `{"model":"test-model","max_tokens":5,"messages":[]}`,
			`{"model":"test-model","messages":[],"max_completion_tokens":5}`, nil},
		"new name already sent": {openAIMaxTokens, nil, `{"model":"test-model","max_tokens":5,"max_completion_tokens":9,"messages":[]}`,
			`{"model":"test-model","max_completion_tokens":9,"messages":[]}`, []string{"dropped_params=max_tokens; reason=superseded_by_rename"}},
		"with a strip": {openAIMaxTokens, []model.Capabilities{{UnsupportedParams: []string{"temperature"}}}, `{"model":"test-model","temperature":1,"max_tokens":5,"max_completion_tokens":9}`,
			`{"model":"test-model","max_completion_tokens":9}`, []string{"dropped_params=temperature; reason=unsupported_by_model", "dropped_params=max_tokens; reason=superseded_by_rename"}},
		"table for another adapter": {map[adapters.Name]map[string]string{adapters.Anthropic: {"max_tokens": "max_completion_tokens"}}, nil, `{"model":"test-model","max_tokens":5}`,
			`{"model":"test-model","max_tokens":5}`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			upURL, last := answeringUpstream(t, "application/json", `{}`)
			cat, pr := buildDispatchCatalog(t, "ollama-self", adapters.OpenAI, tc.caps...)
			setHostSpec(t, cat, host.Spec{BaseURL: upURL, RequestParamRenames: tc.renames})
			d := buildRunnableDeps(t, cat)

			w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(tc.body), ModelName: "test-model"})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if _, got := last(); got != tc.wantUpstream {
				t.Errorf("upstream body = %s, want %s", got, tc.wantUpstream)
			}
			if diff := cmp.Diff(tc.wantWarnings, w.Header().Values(httpheader.HeaderWarnings)); diff != "" {
				t.Errorf("warnings (-want +got):\n%s", diff)
			}
		})
	}
}

// A native-path request is renamed by the host's table for the shape it is sent in, not the binding's.
func TestDispatch_NativePath_RenamesByInboundShape(t *testing.T) {
	upURL, last := answeringUpstream(t, "application/json", `{}`)
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	setHostSpec(t, cat, host.Spec{BaseURL: upURL, RequestParamRenames: map[adapters.Name]map[string]string{
		adapters.OpenAI:          {"max_tokens": "max_completion_tokens"},
		adapters.OpenAIResponses: {"legacy": "current"},
	}})
	d := buildRunnableDeps(t, cat)

	w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAIResponses, Body: []byte(`{"model":"test-model","input":"hi","max_tokens":5,"legacy":1}`), ModelName: "test-model"})

	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if path, got := last(); path != "/v1/responses" || got != `{"model":"test-model","input":"hi","max_tokens":5,"current":1}` {
		t.Errorf("upstream got %s %s", path, got)
	}
}

// A cross-shape request is renamed after it is serialized for the upstream: the Chat Completions max_tokens the serializer writes reaches a renaming host as max_completion_tokens.
func TestDispatch_Canonical_RenamesParams(t *testing.T) {
	const ccReply = `{"id":"c","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
	for name, tc := range map[string]struct {
		renames      map[adapters.Name]map[string]string
		sent, absent string
	}{
		"renaming host": {openAIMaxTokens, "max_completion_tokens", "max_tokens"},
		"plain host":    {nil, "max_tokens", "max_completion_tokens"},
	} {
		t.Run(name, func(t *testing.T) {
			upURL, last := answeringUpstream(t, "application/json", ccReply)
			cat, pr := buildDispatchCatalog(t, "groq", adapters.OpenAI)
			setHostSpec(t, cat, host.Spec{BaseURL: upURL, RequestParamRenames: tc.renames})
			d := buildCrossShapeDeps(t, cat)

			w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAIResponses, Body: []byte(`{"model":"test-model","input":"hi","max_output_tokens":64}`), ModelName: "test-model"})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			_, got := last()
			var sent map[string]json.RawMessage
			if err := json.Unmarshal([]byte(got), &sent); err != nil {
				t.Fatalf("upstream body %s: %v", got, err)
			}
			if string(sent[tc.sent]) != "64" || sent[tc.absent] != nil {
				t.Errorf("upstream body = %s, want %s:64 and no %s", got, tc.sent, tc.absent)
			}
		})
	}
}

func TestStripCanonicalParams(t *testing.T) {
	temp, topP, topK := 0.7, 0.9, 5
	all := func() *v1.SamplingParams { return &v1.SamplingParams{Temperature: &temp, TopP: &topP, TopK: &topK} }
	tests := []struct {
		name        string
		config      map[string]*v1.ModelOpts
		unsupported []string
		wantDropped []string
		wantKept    map[string]v1.SamplingParams
	}{
		{"flagged params go, the rest stay, unknown names are inert",
			map[string]*v1.ModelOpts{"m": {Sampling: all()}}, []string{"temperature", "top_k", "unknown_future_param"},
			[]string{"temperature", "top_k"}, map[string]v1.SamplingParams{"m": {TopP: &topP}}},
		{"every flagged param",
			map[string]*v1.ModelOpts{"m": {Sampling: all()}}, []string{"temperature", "top_p", "top_k"},
			[]string{"temperature", "top_p", "top_k"}, map[string]v1.SamplingParams{"m": {}}},
		{"a param set on several entries is reported once",
			map[string]*v1.ModelOpts{"a": {Sampling: &v1.SamplingParams{Temperature: &temp}}, "b": {Sampling: &v1.SamplingParams{Temperature: &temp}}}, []string{"temperature"},
			[]string{"temperature"}, map[string]v1.SamplingParams{"a": {}, "b": {}}},
		{"a param the request does not set is not reported",
			map[string]*v1.ModelOpts{"m": {Sampling: &v1.SamplingParams{TopP: &topP}}}, []string{"temperature"},
			nil, map[string]v1.SamplingParams{"m": {TopP: &topP}}},
		{"entries without sampling", map[string]*v1.ModelOpts{"m": {}, "n": nil}, []string{"temperature"}, nil, nil},
		{"no model config", nil, []string{"temperature"}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &v1.Request{Model: v1.ModelRefs{"m"}, ModelConfig: tt.config}
			if diff := cmp.Diff(tt.wantDropped, stripCanonicalParams(req, tt.unsupported)); diff != "" {
				t.Errorf("dropped (-want +got):\n%s", diff)
			}
			for name, want := range tt.wantKept {
				if diff := cmp.Diff(want, *req.ModelConfig[name].Sampling); diff != "" {
					t.Errorf("sampling left on %q (-want +got):\n%s", name, diff)
				}
			}
		})
	}
}

// A same-shape request carrying a param the routed model declares unsupported reaches the upstream without it, and the caller is told; a param that is flagged but absent, or present on an unflagged model, changes nothing.
func TestDispatch_BytePass_StripsUnsupportedParams(t *testing.T) {
	const body = `{"model":"test-model","temperature":0.7,"messages":[]}`
	for name, tc := range map[string]struct {
		caps         []model.Capabilities
		wantUpstream string
		wantWarning  string
	}{
		"flagged":   {[]model.Capabilities{{UnsupportedParams: []string{"temperature", "top_p"}}}, `{"model":"test-model","messages":[]}`, "dropped_params=temperature; reason=unsupported_by_model"},
		"unflagged": {nil, body, ""},
	} {
		t.Run(name, func(t *testing.T) {
			upURL, last := answeringUpstream(t, "application/json", `{}`)
			cat, pr := buildDispatchCatalog(t, "ollama-self", adapters.OpenAI, tc.caps...)
			pointHostAt(t, cat, upURL)
			d := buildRunnableDeps(t, cat)

			w := dispatch(d, pr, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(body), ModelName: "test-model"})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			if _, got := last(); got != tc.wantUpstream {
				t.Errorf("upstream body = %s, want %s", got, tc.wantUpstream)
			}
			if got := w.Header().Get(httpheader.HeaderWarnings); got != tc.wantWarning {
				t.Errorf("warnings header = %q, want %q", got, tc.wantWarning)
			}
		})
	}
}
