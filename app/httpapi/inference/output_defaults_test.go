package inference

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	v1 "github.com/wyolet/relay/sdk/v1"
)

// The catalog's max output seeds max tokens only where the caller set none, on the one entry a vendor serializer resolves: the model's own, else the sole entry.
func TestApplyOutputDefaults(t *testing.T) {
	ptr := func(i int) *int { return &i }
	sampled := func(max *int) *v1.ModelOpts { return &v1.ModelOpts{Sampling: &v1.SamplingParams{MaxTokens: max}} }
	for _, tc := range []struct {
		name   string
		config map[string]*v1.ModelOpts
		max    int
		want   map[string]*v1.ModelOpts
	}{
		{"no entry", nil, 8192, map[string]*v1.ModelOpts{"m": sampled(ptr(8192))}},
		{"entry without sampling", map[string]*v1.ModelOpts{"m": {}}, 8192, map[string]*v1.ModelOpts{"m": sampled(ptr(8192))}},
		{"sole entry under another name", map[string]*v1.ModelOpts{"alias": sampled(nil)}, 64000, map[string]*v1.ModelOpts{"alias": sampled(ptr(64000))}},
		{"null sole entry under another name", map[string]*v1.ModelOpts{"alias": nil}, 64000, map[string]*v1.ModelOpts{"alias": sampled(ptr(64000))}},
		{"caller's value kept", map[string]*v1.ModelOpts{"m": sampled(ptr(100))}, 64000, map[string]*v1.ModelOpts{"m": sampled(ptr(100))}},
		{"model declares no max", nil, 0, nil},
		{"several entries are left alone", map[string]*v1.ModelOpts{"m": sampled(nil), "b": sampled(nil)}, 8192, map[string]*v1.ModelOpts{"m": sampled(nil), "b": sampled(nil)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &v1.Request{Model: v1.ModelRefs{"m"}, ModelConfig: tc.config}
			applyOutputDefaults(req, tc.max)
			if diff := cmp.Diff(tc.want, req.ModelConfig); diff != "" {
				t.Errorf("model config (-want +got):\n%s", diff)
			}
		})
	}
}

func TestApplyOutputDefaults_NullEntry(t *testing.T) {
	req, err := v1.Parse([]byte(`{"model":"m","model_config":{"m":null},"input":"hi"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	applyOutputDefaults(req, 8192)
	if o := req.ModelConfig["m"]; o == nil || o.Sampling == nil || o.Sampling.MaxTokens == nil || *o.Sampling.MaxTokens != 8192 {
		t.Fatalf("model config = %+v, want max tokens seeded", req.ModelConfig)
	}
}
