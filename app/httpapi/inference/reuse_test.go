package inference

import (
	"errors"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/metrics"
)

// The batch path parses and rewrites an item as the realtime routes do: the same bodies refused, the model renamed, the model's unsupported params stripped and counted.
func TestParseRequestAndUpstreamBody(t *testing.T) {
	if _, err := ParseRequest([]byte(duplicateModelBody)); !errors.Is(err, errDuplicateModel) {
		t.Fatalf("duplicate model: err = %v, want errDuplicateModel", err)
	}
	if req, err := ParseRequest([]byte(`null`)); err != nil || req.Model != "" || string(req.Body()) != "null" {
		t.Fatalf("null body: (%+v, %v), want an empty request", req, err)
	} else if out, dropped := UpstreamBody(req, "up", nil, ccPaths, maxTokensRename); string(out) != "null" || dropped != nil {
		t.Fatalf("null body sent as %s, dropped %v", out, dropped)
	}

	// The stream key is the route's to hold to a spelling; the batch parse reads none.
	req, err := ParseRequest([]byte(`{"model":"m","temperature":0.2,"top_p":1,"top_k":5,"stream":true,"Stream":false}`))
	if err != nil || req.Model != "m" || req.Stream {
		t.Fatalf("parse: (%+v, %v)", req, err)
	}
	m := &model.Model{Meta: meta.Metadata{Name: "reuse-test-model"}}
	if out, dropped := UpstreamBody(req, "up", m, ccPaths, nil); string(out) != `{"model":"up","temperature":0.2,"top_p":1,"top_k":5,"stream":true,"Stream":false}` || dropped != nil {
		t.Fatalf("model with nothing unsupported: sent %s, dropped %v", out, dropped)
	}
	m.Spec.Capabilities.UnsupportedParams = []string{"temperature", "top_k", "seed"}
	out, dropped := UpstreamBody(req, "up", m, ccPaths, nil)
	if string(out) != `{"model":"up","top_p":1,"top_k":5,"stream":true,"Stream":false}` || !slices.Equal(dropped, []string{"temperature"}) {
		t.Fatalf("sent %s, dropped %v; want temperature alone removed: top_k has no path in this shape", out, dropped)
	}
	if got := testutil.ToFloat64(metrics.DroppedParams.WithLabelValues("temperature", "reuse-test-model", metrics.DropUnsupportedByModel)); got != 1 {
		t.Fatalf("dropped-param counter = %v, want 1", got)
	}

	// The host's renames apply after the strip; a renamed key the caller also sent under its new name is dropped and counted.
	req, _ = ParseRequest([]byte(`{"model":"m","max_tokens":5,"temperature":0.2}`))
	if out, dropped := UpstreamBody(req, "up", m, ccPaths, maxTokensRename); string(out) != `{"model":"up","max_completion_tokens":5}` || !slices.Equal(dropped, []string{"temperature"}) {
		t.Fatalf("renamed: sent %s, dropped %v", out, dropped)
	}
	req, _ = ParseRequest([]byte(`{"model":"m","max_tokens":5,"max_completion_tokens":9}`))
	if out, dropped := UpstreamBody(req, "up", m, ccPaths, maxTokensRename); string(out) != `{"model":"up","max_completion_tokens":9}` || !slices.Equal(dropped, []string{"max_tokens"}) {
		t.Fatalf("superseded: sent %s, dropped %v", out, dropped)
	}
	if got := testutil.ToFloat64(metrics.DroppedParams.WithLabelValues("max_tokens", "reuse-test-model", metrics.DropSupersededByRename)); got != 1 {
		t.Fatalf("superseded counter = %v, want 1", got)
	}
}
