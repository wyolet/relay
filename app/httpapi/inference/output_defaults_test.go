package inference

import (
	"testing"

	v1 "github.com/wyolet/relay/pkg/relay/v1"
)

func TestApplyOutputDefaults(t *testing.T) {
	ptr := func(i int) *int { return &i }

	t.Run("seeds max from model when caller unset", func(t *testing.T) {
		req := &v1.Request{
			Model:       v1.ModelRefs{"claude-3-7-sonnet-20250219"},
			ModelConfig: map[string]*v1.ModelOpts{"claude-3-7-sonnet": {Sampling: &v1.SamplingParams{}}},
		}
		applyOutputDefaults(req, 64000)
		// single-entry fallback: the inbound-keyed entry is the one a vendor
		// SerializeRequest resolves, so it must be the one we modified.
		got := req.ModelConfig["claude-3-7-sonnet"].Sampling.MaxTokens
		if got == nil || *got != 64000 {
			t.Fatalf("MaxTokens = %v, want 64000", got)
		}
	})

	t.Run("does not overwrite caller value", func(t *testing.T) {
		req := &v1.Request{
			Model:       v1.ModelRefs{"m"},
			ModelConfig: map[string]*v1.ModelOpts{"m": {Sampling: &v1.SamplingParams{MaxTokens: ptr(100)}}},
		}
		applyOutputDefaults(req, 64000)
		if *req.ModelConfig["m"].Sampling.MaxTokens != 100 {
			t.Fatalf("caller MaxTokens overwritten")
		}
	})

	t.Run("no model max is a no-op", func(t *testing.T) {
		req := &v1.Request{Model: v1.ModelRefs{"m"}}
		applyOutputDefaults(req, 0)
		if len(req.ModelConfig) != 0 {
			t.Fatalf("ModelConfig mutated on zero max: %v", req.ModelConfig)
		}
	})

	t.Run("creates opts when none exist", func(t *testing.T) {
		req := &v1.Request{Model: v1.ModelRefs{"m"}}
		applyOutputDefaults(req, 8192)
		o := req.ModelConfig["m"]
		if o == nil || o.Sampling == nil || o.Sampling.MaxTokens == nil || *o.Sampling.MaxTokens != 8192 {
			t.Fatalf("expected created opts with MaxTokens 8192, got %+v", o)
		}
	})

	t.Run("skips multiplex (multiple entries)", func(t *testing.T) {
		req := &v1.Request{
			Model: v1.ModelRefs{"a"},
			ModelConfig: map[string]*v1.ModelOpts{
				"a": {Sampling: &v1.SamplingParams{}},
				"b": {Sampling: &v1.SamplingParams{}},
			},
		}
		applyOutputDefaults(req, 8192)
		if req.ModelConfig["a"].Sampling.MaxTokens != nil {
			t.Fatalf("multiplex should be skipped")
		}
	})
}
