package control

import (
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
)

func overlayTmpl() *model.Model {
	return &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "claude-fable-5",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: meta.NewID()}},
		Spec: model.Spec{
			Snapshots: []model.Snapshot{{Name: "claude-fable-5"}},
			Pointer:   "claude-fable-5",
		},
	}
}

func TestBuildOverlayView(t *testing.T) {
	tmpl := overlayTmpl()

	t.Run("no overlay — template passthrough", func(t *testing.T) {
		v := buildOverlayView(tmpl, nil)
		if v.Patch != nil || v.Quarantined || v.Effective.Family != tmpl.Spec.Family {
			t.Fatalf("passthrough view wrong: %+v", v)
		}
	})

	t.Run("valid overlay — effective differs, template pristine", func(t *testing.T) {
		o := &overlay.Overlay{Kind: overlay.KindModel, ResourceID: "",
			Patch: []byte(`{"aliases":["nick"],"family":"custom"}`)}
		o.ResourceID = tmpl.Meta.ID
		v := buildOverlayView(tmpl, o)
		if v.Quarantined {
			t.Fatalf("unexpected quarantine: %s", v.QuarantineReason)
		}
		if v.Effective.Family != "custom" || len(v.Effective.Aliases) != 1 {
			t.Errorf("effective not merged: %+v", v.Effective)
		}
		if v.Template.Family != "" || len(v.Template.Aliases) != 0 {
			t.Errorf("template not pristine: %+v", v.Template)
		}
	})

	t.Run("invalid merge — quarantined, template served", func(t *testing.T) {
		o := &overlay.Overlay{Kind: overlay.KindModel, ResourceID: "",
			Patch: []byte(`{"pointer":"nope"}`)}
		o.ResourceID = tmpl.Meta.ID
		v := buildOverlayView(tmpl, o)
		if !v.Quarantined || v.QuarantineReason == "" {
			t.Fatal("quarantine state not surfaced")
		}
		if v.Effective.Pointer != tmpl.Spec.Pointer {
			t.Error("quarantined view must serve the template spec")
		}
	})
}
