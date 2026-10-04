// Package fidelity is test support for vendor adapters: it builds a canonical
// request with every request-level field set to a distinctive value, and
// checks a serialized body either carries each field or names it in an
// explicit drop list. A canonical field added later without a fixture value
// fails this package's own test; one an adapter neither emits nor lists fails
// that adapter's test — so no request field drops silently (rule 11).
//
// Scope: Request and the config structs under it (ModelOpts, CacheConfig,
// ToolsConfig, FunctionTool). Input items and responses are out of scope —
// adapters cover those with per-item round-trip tests.
package fidelity

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// Model is the fixture's model name (and its ModelConfig key).
const Model = "fidelity-model"

// field is one canonical request field: its dotted path and the substring a
// body carries when the field is emitted. An empty want means the value has
// no vendor-neutral spelling (booleans, enums, durations), so each adapter
// must give the wire form or list the field as dropped.
type field struct {
	path string
	want string
}

var fields = []field{
	{"Instructions", "fidelity-instructions"},
	{"Model", Model},
	{"ModelConfig.Sampling.Temperature", "0.37"},
	{"ModelConfig.Sampling.TopP", "0.83"},
	{"ModelConfig.Sampling.TopK", "77"},
	{"ModelConfig.Sampling.MaxTokens", "4321"},
	{"ModelConfig.Sampling.Stop", "fidelity-stop"},
	{"ModelConfig.Sampling.Seed", "98765"},
	{"ModelConfig.Sampling.FrequencyPenalty", "0.29"},
	{"ModelConfig.Sampling.PresencePenalty", "0.19"},
	{"ModelConfig.Reasoning.Effort", ""},
	{"ModelConfig.Reasoning.Summary", ""},
	{"ModelConfig.Reasoning.BudgetTokens", "2345"},
	{"ModelConfig.Output.Format.Type", ""},
	{"ModelConfig.Output.Format.Name", "fidelity_format"},
	{"ModelConfig.Output.Format.Description", "fidelity format description"},
	{"ModelConfig.Output.Format.Schema", "fidelity_field"},
	{"ModelConfig.Output.Format.Strict", ""},
	{"ModelConfig.Output.Verbosity", ""},
	{"Tools.Definitions.Name", "fidelity_tool"},
	{"Tools.Definitions.Description", "fidelity tool description"},
	{"Tools.Definitions.Parameters", "fidelity_param"},
	{"Tools.Definitions.Strict", ""},
	{"Tools.Definitions.ProviderData", "fidelity-provider-data"},
	{"Tools.Choice.Mode", ""},
	{"Tools.Choice.FunctionName", ""},
	{"Tools.Parallel", ""},
	{"CacheConfig.Instructions", ""},
	{"CacheConfig.Tools", ""},
	{"CacheConfig.Key", "fidelity-cache-key"},
	{"CacheConfig.TTL", ""},
	{"OutputMode", ""},
	{"User", "fidelity-user"},
	{"Metadata", "fidelity-meta-value"},
	{"Extensions", "fidelity-extension"},
}

// Request returns a fresh fully-populated canonical request. Effort is
// "medium", Summary "detailed", Verbosity "low", Format.Type "json_schema",
// Format.Strict false, tool Strict true, Choice forces fidelity_tool,
// Parallel false, both cache flags on with TTL "2h", OutputMode stream.
func Request() *v1.Request {
	f := func(v float64) *float64 { return &v }
	i := func(v int) *int { return &v }
	b := func(v bool) *bool { return &v }
	return &v1.Request{
		Model:        v1.ModelRefs{Model},
		Instructions: "fidelity-instructions",
		Input: []v1.Item{&v1.Message{
			Role:    v1.RoleUser,
			Content: []v1.Part{&v1.TextPart{Text: "hello"}},
		}},
		ModelConfig: map[string]*v1.ModelOpts{Model: {
			Sampling: &v1.SamplingParams{
				Temperature:      f(0.37),
				TopP:             f(0.83),
				TopK:             i(77),
				MaxTokens:        i(4321),
				Stop:             []string{"fidelity-stop"},
				Seed:             i(98765),
				FrequencyPenalty: f(0.29),
				PresencePenalty:  f(0.19),
			},
			Reasoning: &v1.ReasoningConfig{Effort: "medium", Summary: "detailed", BudgetTokens: i(2345)},
			Output: &v1.OutputConfig{
				Format: &v1.Format{
					Type:        "json_schema",
					Name:        "fidelity_format",
					Description: "fidelity format description",
					Schema:      json.RawMessage(`{"type":"object","properties":{"fidelity_field":{"type":"string"}}}`),
					Strict:      b(false),
				},
				Verbosity: "low",
			},
		}},
		Tools: &v1.ToolsConfig{
			Definitions: v1.Tools{&v1.FunctionTool{
				Name:         "fidelity_tool",
				Description:  "fidelity tool description",
				Parameters:   json.RawMessage(`{"type":"object","properties":{"fidelity_param":{"type":"string"}}}`),
				Strict:       b(true),
				ProviderData: json.RawMessage(`{"type":"function","note":"fidelity-provider-data"}`),
			}},
			Choice:   &v1.ToolChoice{Mode: "function", FunctionName: "fidelity_tool"},
			Parallel: b(false),
		},
		CacheConfig: &v1.CacheConfig{Instructions: true, Tools: true, Key: "fidelity-cache-key", TTL: "2h"},
		OutputMode:  v1.OutputModeStream,
		User:        "fidelity-user",
		Metadata:    map[string]string{"fidelity_meta": "fidelity-meta-value"},
		Extensions:  map[string]json.RawMessage{"fidelity.unowned": json.RawMessage(`"fidelity-extension"`)},
	}
}

// Check asserts that every canonical request field is either on the wire —
// body contains wire[path], or the field's default substring — or named in
// dropped (path → why; mirror the adapter's `// canonical:` annotation). A
// field listed as dropped whose distinctive value still appears in the body
// fails too, so the drop list cannot go stale.
func Check(t testing.TB, body []byte, wire, dropped map[string]string) {
	t.Helper()
	s := string(body)
	known := map[string]bool{}
	for _, f := range fields {
		known[f.path] = true
		_, isWire := wire[f.path]
		why, isDropped := dropped[f.path]
		switch {
		case isWire && isDropped:
			t.Errorf("%s: listed both as emitted and as dropped", f.path)
		case isDropped:
			if strings.TrimSpace(why) == "" {
				t.Errorf("%s: dropped without a reason", f.path)
			}
			if f.want != "" && strings.Contains(s, f.want) {
				t.Errorf("%s: listed as dropped but %q is on the wire", f.path, f.want)
			}
		default:
			want := f.want
			if isWire {
				want = wire[f.path]
			}
			if want == "" {
				t.Errorf("%s: no wire form given and not listed as dropped — emit it or annotate the drop", f.path)
				continue
			}
			if !strings.Contains(s, want) {
				t.Errorf("%s: %q not on the wire and not listed as dropped", f.path, want)
			}
		}
	}
	for _, m := range []map[string]string{wire, dropped} {
		for p := range m {
			if !known[p] {
				t.Errorf("%s: not a canonical request field", p)
			}
		}
	}
	if t.Failed() {
		t.Logf("body: %s", body)
	}
}
