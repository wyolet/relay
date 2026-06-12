package overlay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
)

func mustMerge(t *testing.T, tmpl, patch string) map[string]any {
	t.Helper()
	out, err := MergeSpec(KindModel, []byte(tmpl), []byte(patch))
	if err != nil {
		t.Fatalf("MergeSpec: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("effective not an object: %v", err)
	}
	return m
}

func TestMergeSpec_ScalarsReplaceNullDeletes(t *testing.T) {
	eff := mustMerge(t,
		`{"family":"claude","version":"1","maxOutputTokens":128000}`,
		`{"version":"2","maxOutputTokens":null,"documentation":"https://x"}`)
	if eff["family"] != "claude" || eff["version"] != "2" || eff["documentation"] != "https://x" {
		t.Errorf("scalar merge wrong: %v", eff)
	}
	if _, exists := eff["maxOutputTokens"]; exists {
		t.Error("null did not delete the field")
	}
}

func TestMergeSpec_NestedObjectsRecurse(t *testing.T) {
	eff := mustMerge(t,
		`{"capabilities":{"chat":true,"tools":true}}`,
		`{"capabilities":{"vision":true}}`)
	caps := eff["capabilities"].(map[string]any)
	if caps["chat"] != true || caps["tools"] != true || caps["vision"] != true {
		t.Errorf("nested merge lost fields: %v", caps)
	}
}

func TestMergeSpec_PlainArraysReplace(t *testing.T) {
	// snapshots is a keyed-object list — NOT in the union whitelist.
	eff := mustMerge(t,
		`{"snapshots":[{"name":"a"},{"name":"b"}]}`,
		`{"snapshots":[{"name":"c"}]}`)
	if got := len(eff["snapshots"].([]any)); got != 1 {
		t.Errorf("non-union array should replace wholesale, got %d entries", got)
	}
}

func TestMergeSpec_AliasesUnionWithNormalizedDedup(t *testing.T) {
	eff := mustMerge(t,
		`{"aliases":["claude-fable-5[1m]"]}`,
		`{"aliases":["my-fast-model","claude.fable.5[1M]","claude-fable-5[2m]"]}`)
	got := eff["aliases"].([]any)
	// claude.fable.5[1M] normalizes identically to the template's entry →
	// deduped; template order preserved first.
	want := []string{"claude-fable-5[1m]", "my-fast-model", "claude-fable-5[2m]"}
	if len(got) != len(want) {
		t.Fatalf("union: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("union[%d]: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestMergeSpec_TagsUnionCaseInsensitive(t *testing.T) {
	eff := mustMerge(t,
		`{"tags":["Fast"]}`,
		`{"tags":["fast","cheap"]}`)
	if got := len(eff["tags"].([]any)); got != 2 {
		t.Errorf("tags union: got %d entries, want 2", got)
	}
}

func TestMergeSpec_UnionFieldMissingOnTemplate(t *testing.T) {
	eff := mustMerge(t, `{"family":"claude"}`, `{"aliases":["nick"]}`)
	if got := len(eff["aliases"].([]any)); got != 1 {
		t.Errorf("union onto missing template field: %v", eff["aliases"])
	}
}

func TestValidate_Rules(t *testing.T) {
	ok := &Overlay{Kind: KindModel, ResourceID: "id1", Patch: []byte(`{"aliases":["x"]}`)}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid overlay rejected: %v", err)
	}
	for name, o := range map[string]*Overlay{
		"bad kind":    {Kind: "host", ResourceID: "id1", Patch: []byte(`{"a":1}`)},
		"no resource": {Kind: KindModel, Patch: []byte(`{"a":1}`)},
		"non-object":  {Kind: KindModel, ResourceID: "id1", Patch: []byte(`[1]`)},
		"empty patch": {Kind: KindModel, ResourceID: "id1", Patch: []byte(`{}`)},
		"enabled key": {Kind: KindModel, ResourceID: "id1", Patch: []byte(`{"enabled":false}`)},
	} {
		if err := o.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestEffectiveModel_MergeAndQuarantinePath(t *testing.T) {
	tmpl := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "claude-fable-5",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: meta.NewID()}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "claude-fable-5"}},
			Pointer:   "claude-fable-5",
			Aliases:   []string{"claude-fable-5[1m]"},
		},
	}

	o := &Overlay{Kind: KindModel, ResourceID: tmpl.Meta.ID,
		Patch: []byte(`{"aliases":["my-fable"],"family":"claude"}`)}
	eff, err := EffectiveModel(tmpl, o)
	if err != nil {
		t.Fatalf("EffectiveModel: %v", err)
	}
	if eff.Meta.ID != tmpl.Meta.ID || eff.Meta.Name != tmpl.Meta.Name {
		t.Error("identity changed")
	}
	if len(eff.Spec.Aliases) != 2 || eff.Spec.Family != "claude" {
		t.Errorf("merge wrong: %+v", eff.Spec)
	}
	if len(tmpl.Spec.Aliases) != 1 || tmpl.Spec.Family != "" {
		t.Error("template mutated")
	}

	// Invalid effective (pattern alias with no literal prefix) → error,
	// the caller's quarantine signal.
	bad := &Overlay{Kind: KindModel, ResourceID: tmpl.Meta.ID, Patch: []byte(`{"aliases":["*nope"]}`)}
	if _, err := EffectiveModel(tmpl, bad); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("want effective-invalid error, got %v", err)
	}

	// Nil overlay → template passthrough.
	if got, err := EffectiveModel(tmpl, nil); err != nil || got != tmpl {
		t.Fatalf("nil overlay passthrough: %v %v", got, err)
	}
}
