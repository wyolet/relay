package inference

import (
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/httpheader"
	v1 "github.com/wyolet/relay/sdk/v1"
)

var ccPaths = map[string]string{"temperature": "temperature", "top_p": "top_p"}

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
