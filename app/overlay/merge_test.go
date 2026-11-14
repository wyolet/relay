package overlay

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/meta"
)

// errText is err's message up to the decoder's own wording, which is the standard library's to change.
func errText(err error) string {
	if err == nil {
		return ""
	}
	msg, _, _ := strings.Cut(err.Error(), ": json: ")
	msg, _, _ = strings.Cut(msg, ": unexpected end of JSON input")
	return msg
}

// Scalars replace, null deletes, objects merge, arrays replace whole; aliases and tags are the union, the template's entries first, deduplicated on the form the resolver matches on.
func TestMergeSpec(t *testing.T) {
	for _, tc := range []struct {
		name, kind, tmpl, patch string
		want, wantErr           string
	}{
		{"scalars replace, null deletes, new keys land", KindModel,
			`{"family":"claude","version":"1","maxOutputTokens":128000}`, `{"version":"2","maxOutputTokens":null,"documentation":"https://x"}`,
			`{"documentation":"https://x","family":"claude","version":"2"}`, ""},
		{"nested objects recurse", KindModel,
			`{"capabilities":{"chat":true,"tools":true}}`, `{"capabilities":{"vision":true,"tools":null}}`,
			`{"capabilities":{"chat":true,"vision":true}}`, ""},
		{"an object replaces a scalar and a scalar an object", KindModel,
			`{"a":1,"b":{"x":1}}`, `{"a":{"y":2},"b":3}`, `{"a":{"y":2},"b":3}`, ""},
		{"arrays outside the union fields replace whole", KindModel,
			`{"snapshots":[{"name":"a"},{"name":"b"}]}`, `{"snapshots":[{"name":"c"}]}`, `{"snapshots":[{"name":"c"}]}`, ""},
		{"aliases union on the normalised form", KindModel,
			`{"aliases":["claude-fable-5[1m]"]}`, `{"aliases":["my-fast-model","claude.fable.5[1M]","claude-fable-5[2m]","my-fast-model"]}`,
			`{"aliases":["claude-fable-5[1m]","my-fast-model","claude-fable-5[2m]"]}`, ""},
		{"tags union ignoring case", KindModel, `{"tags":["Fast"]}`, `{"tags":["fast","cheap"]}`, `{"tags":["Fast","cheap"]}`, ""},
		{"a union field the template lacks", KindModel, `{"family":"claude"}`, `{"aliases":["nick"]}`, `{"aliases":["nick"],"family":"claude"}`, ""},
		{"a union field patched with a non-list replaces", KindModel, `{"aliases":["a"]}`, `{"aliases":"b"}`, `{"aliases":"b"}`, ""},
		{"a union field patched with non-strings replaces", KindModel, `{"aliases":["a"]}`, `{"aliases":[1]}`, `{"aliases":[1]}`, ""},
		{"a union field nested in an object replaces", KindModel, `{"x":{"aliases":["a"]}}`, `{"x":{"aliases":["b"]}}`, `{"x":{"aliases":["b"]}}`, ""},
		{"another kind has no union fields", "host", `{"aliases":["a"]}`, `{"aliases":["b"]}`, `{"aliases":["b"]}`, ""},
		{"a null template", KindModel, `null`, `{"family":"x"}`, `{"family":"x"}`, ""},
		{"a template that is no object", KindModel, `[1]`, `{}`, "", "overlay merge: template spec"},
		{"a patch that is no object", KindModel, `{}`, `"x"`, "", "overlay merge: patch"},
	} {
		out, err := MergeSpec(tc.kind, []byte(tc.tmpl), []byte(tc.patch))
		if string(out) != tc.want {
			t.Errorf("%s: effective %s, want %s", tc.name, out, tc.want)
		}
		if got := errText(err); got != tc.wantErr {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.wantErr)
		}
	}
}

func TestEffectiveModel(t *testing.T) {
	tmpl := &model.Model{
		Meta: meta.Metadata{ID: "01a19bc3-0000-7000-8000-000000000001", Name: "claude-fable-5", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: "01a19bc3-0000-7000-8000-000000000002"}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "claude-fable-5"}}, Pointer: "claude-fable-5", Aliases: []string{"claude-fable-5[1m]"}},
	}
	before := *tmpl
	before.Spec.Aliases = []string{"claude-fable-5[1m]"}

	eff, err := EffectiveModel(tmpl, &Overlay{Kind: KindModel, ResourceID: tmpl.Meta.ID, Patch: []byte(`{"aliases":["my-fable"],"family":"claude"}`)})
	if err != nil {
		t.Fatalf("EffectiveModel: %v", err)
	}
	want := before
	want.Spec.Aliases, want.Spec.Family = []string{"claude-fable-5[1m]", "my-fable"}, "claude"
	if diff := cmp.Diff(&want, eff); diff != "" {
		t.Errorf("effective model (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(&before, tmpl); diff != "" {
		t.Errorf("the template was mutated (-before +after):\n%s", diff)
	}

	// The caller quarantines an overlay whose effective row is invalid, here an alias pattern with no literal prefix.
	for _, tc := range []struct{ name, patch, want string }{
		{"an invalid effective row", `{"aliases":["*nope"]}`, `overlay: effective row invalid: model "claude-fable-5": alias "*nope": pattern must start with a literal prefix`},
		{"a patch whose value does not decode into the spec", `{"pointer":5}`, "overlay: effective spec does not decode"},
		{"a patch that is no object", `[]`, "overlay merge: patch"},
	} {
		_, err := EffectiveModel(tmpl, &Overlay{Kind: KindModel, ResourceID: tmpl.Meta.ID, Patch: []byte(tc.patch)})
		if got := errText(err); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
	if got, err := EffectiveModel(tmpl, nil); err != nil || got != tmpl {
		t.Errorf("no overlay: %v, %v; want the template itself", got, err)
	}
	if _, err := EffectiveModel(nil, nil); errText(err) != "overlay: template model is nil" {
		t.Errorf("no template: err = %v", err)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Overlay
		want string
	}{
		{"a model patch", Overlay{Kind: KindModel, ResourceID: "id1", Patch: []byte(`{"aliases":["x"]}`)}, ""},
		{"another kind", Overlay{Kind: "host", ResourceID: "id1", Patch: []byte(`{"a":1}`)}, `overlay: unsupported kind "host" (v1 supports "model")`},
		{"no resource", Overlay{Kind: KindModel, Patch: []byte(`{"a":1}`)}, "overlay: resourceId is required"},
		{"a patch that is no object", Overlay{Kind: KindModel, ResourceID: "id1", Patch: []byte(`[1]`)}, "overlay: patch must be a JSON object"},
		{"an empty patch", Overlay{Kind: KindModel, ResourceID: "id1", Patch: []byte(`{}`)}, "overlay: patch is empty — delete the overlay instead"},
		{"no patch", Overlay{Kind: KindModel, ResourceID: "id1"}, "overlay: patch must be a JSON object"},
		{"a field no overlay patches", Overlay{Kind: KindModel, ResourceID: "id1", Patch: []byte(`{"enabled":false}`)}, `overlay: field "enabled" is not patchable via overlay — edit the resource directly`},
	} {
		if got := errText(tc.o.Validate()); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
	if got := (&Overlay{Kind: KindModel, ResourceID: "id1"}).Key(); got != "model|id1" || TargetKey("model", "id1") != got {
		t.Errorf("Key = %q, want model|id1", got)
	}
}
