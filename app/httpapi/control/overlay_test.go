package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/meta"
)

func TestBuildOverlayView(t *testing.T) {
	spec := func() model.Spec {
		return model.Spec{Snapshots: []model.Snapshot{{Name: "claude-fable-5"}}, Pointer: "claude-fable-5", Aliases: []string{"fable"}}
	}
	id := meta.NewID()
	tmpl := &model.Model{Meta: meta.Metadata{ID: id, Name: "claude-fable-5", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: meta.NewID()}}, Spec: spec()}
	at := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	merged := spec()
	merged.Family, merged.Aliases = "custom", []string{"fable", "nick"}

	for _, tc := range []struct {
		name  string
		patch string
		want  overlayView
	}{
		{name: "no overlay serves the template", want: overlayView{Effective: spec()}},
		{name: "a patch merges into effective", patch: `{"aliases":["nick"],"family":"custom"}`, want: overlayView{Effective: merged}},
		{name: "a patch that no longer merges is quarantined", patch: `{"pointer":"nope"}`, want: overlayView{Effective: spec(), Quarantined: true, QuarantineReason: `overlay: effective row invalid: model "claude-fable-5": pointer "nope" does not match any snapshot`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var o *overlay.Overlay
			tc.want.Kind, tc.want.ResourceID, tc.want.Template = "model", id, spec()
			if tc.patch != "" {
				o = &overlay.Overlay{Kind: overlay.KindModel, ResourceID: id, Patch: json.RawMessage(tc.patch), UpdatedAt: at}
				tc.want.Patch, tc.want.UpdatedAt = json.RawMessage(tc.patch), at
			}
			if diff := cmp.Diff(tc.want, buildOverlayView(tmpl, o)); diff != "" {
				t.Errorf("view (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(spec(), tmpl.Spec); diff != "" {
				t.Errorf("the merge changed the template (-want +got):\n%s", diff)
			}
		})
	}
}
