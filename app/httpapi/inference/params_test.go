package inference

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/pkg/httpheader"
	v1 "github.com/wyolet/relay/sdk/v1"
)

var ccPaths = map[string]string{"temperature": "temperature", "top_p": "top_p"}

func TestStripCanonicalParams(t *testing.T) {
	temp, topP := 0.7, 0.9
	topK := 5
	req := &v1.Request{
		Model: v1.ModelRefs{"m"},
		ModelConfig: map[string]*v1.ModelOpts{
			"m": {Sampling: &v1.SamplingParams{Temperature: &temp, TopP: &topP, TopK: &topK}},
		},
	}
	dropped := stripCanonicalParams(req, []string{"temperature", "top_k", "unknown_future_param"})
	if len(dropped) != 2 {
		t.Fatalf("dropped = %v, want temperature+top_k", dropped)
	}
	s := req.ModelConfig["m"].Sampling
	if s.Temperature != nil || s.TopK != nil {
		t.Error("flagged params survived the strip")
	}
	if s.TopP == nil {
		t.Error("unflagged top_p was stripped")
	}
}

func TestStripCanonicalParamsNothingSet(t *testing.T) {
	req := &v1.Request{Model: v1.ModelRefs{"m"}}
	if dropped := stripCanonicalParams(req, []string{"temperature"}); dropped != nil {
		t.Fatalf("dropped = %v, want none", dropped)
	}
}

// TestDispatch_BytePass_StripsUnsupportedParams drives the byte-pass guard
// through Dispatch: a same-shape request carrying temperature to a model
// flagged unsupported must surface the drop via the warnings header (set
// before the pipeline runs, so no live upstream is needed). top_p is also
// flagged but absent from the body, so it must not be reported dropped.
func TestDispatch_BytePass_StripsUnsupportedParams(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "ollama-self", adapters.OpenAI,
		model.Capabilities{UnsupportedParams: []string{"temperature", "top_p"}})
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)
	r = withNormalContext(r, pr)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"test-model","temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`),
		ModelName: "test-model",
	})

	warn := w.Header().Get(httpheader.HeaderWarnings)
	if !strings.Contains(warn, "temperature") {
		t.Fatalf("warnings header = %q, want dropped temperature", warn)
	}
	if strings.Contains(warn, "top_p") {
		t.Errorf("warnings header = %q reports top_p, which was not in the body", warn)
	}
}

// TestDispatch_BytePass_UnflaggedModelUntouched: no capability flags → the
// guard must not fire at all, temperature or not.
func TestDispatch_BytePass_UnflaggedModelUntouched(t *testing.T) {
	cat, pr := buildDispatchCatalog(t, "ollama-self", adapters.OpenAI)
	d := buildDeps(t, cat)

	r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil)
	r = withNormalContext(r, pr)
	w := httptest.NewRecorder()

	Dispatch(d, w, r, DispatchInput{
		Inbound:   adapters.OpenAI,
		Body:      []byte(`{"model":"test-model","temperature":0.7,"messages":[]}`),
		ModelName: "test-model",
	})

	if warn := w.Header().Get(httpheader.HeaderWarnings); warn != "" {
		t.Fatalf("warnings header = %q on an unflagged model, want none", warn)
	}
}
