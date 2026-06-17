// overlay.go binds the catalog-overlay subresource (design/overlays.md):
//
//	GET    /models/by-id/{id}/overlay   patch + template + effective + quarantine state
//	PUT    /models/by-id/{id}/overlay   set/replace the user's sparse patch
//	DELETE /models/by-id/{id}/overlay   factory reset (template resumes verbatim)
//
// The overlay is an explicitly user-managed patch document — PUT replaces
// the whole patch. Writes hit PG only; the table's NOTIFY trigger fans the
// merge out to every pod's snapshot (~1s), same as generic CRUD.
package control

import (
	"context"
	"encoding/json"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/settings"
)

type overlayIDInput struct {
	ID string `path:"id" doc:"Model UUIDv7 id."`
}

type overlayPutInput struct {
	ID   string `path:"id" doc:"Model UUIDv7 id."`
	Body struct {
		// Patch is a sparse JSON object of spec fields. Union fields
		// (aliases, tags) merge with the template; everything else
		// replaces. `enabled` is not patchable.
		Patch json.RawMessage `json:"patch" doc:"Sparse spec patch (JSON object)."`
	}
}

type overlayView struct {
	Kind       string `json:"kind"`
	ResourceID string `json:"resourceId"`
	// Patch is the stored user patch; null when no overlay exists.
	Patch json.RawMessage `json:"patch,omitempty"`
	// Template is the pristine catalog spec (what re-seed maintains).
	Template model.Spec `json:"template"`
	// Effective is the merged spec the snapshot serves. Equals Template
	// when no overlay exists or the overlay is quarantined.
	Effective model.Spec `json:"effective"`
	// Quarantined means the patch no longer merges into a valid row
	// (usually after a template change); the pristine template is being
	// served. Fix or delete the patch.
	Quarantined      bool      `json:"quarantined,omitempty"`
	QuarantineReason string    `json:"quarantineReason,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt,omitempty"`
}

type overlayOut struct {
	Body overlayView
}

type overlayDeleteOut struct {
	Body struct {
		Deleted bool `json:"deleted"`
	}
}

// buildOverlayView computes the provenance view: template from PG (the
// PG row IS the template — effective state lives only in the snapshot),
// merge recomputed live so the response always reflects current truth.
func buildOverlayView(tmpl *model.Model, o *overlay.Overlay) overlayView {
	v := overlayView{
		Kind:       overlay.KindModel,
		ResourceID: tmpl.Meta.ID,
		Template:   tmpl.Spec,
		Effective:  tmpl.Spec,
	}
	if o == nil {
		return v
	}
	v.Patch = o.Patch
	v.UpdatedAt = o.UpdatedAt
	eff, err := overlay.EffectiveModel(tmpl, o)
	if err != nil {
		v.Quarantined = true
		v.QuarantineReason = err.Error()
		return v
	}
	v.Effective = eff.Spec
	return v
}

func registerOverlayRoutes(api huma.API, d Deps, protect huma.Middlewares) {
	getTemplate := func(ctx context.Context, id string) (*model.Model, error) {
		tmpl, err := d.Stores.Model.Get(ctx, id)
		if err != nil || tmpl == nil {
			return nil, huma.Error404NotFound("model with id " + id + " not found")
		}
		return tmpl, nil
	}

	huma.Register(api, huma.Operation{
		OperationID: "model_overlay_get", Method: "GET", Path: "/models/by-id/{id}/overlay",
		Summary: "Read this model's overlay: patch, pristine template, effective spec, quarantine state",
		Tags:    []string{"models"}, Middlewares: protect, Errors: []int{401, 404},
	}, func(ctx context.Context, in *overlayIDInput) (*overlayOut, error) {
		if err := d.Authz.Authorize(ctx, "models.overlay.read", authz.Resource{Kind: "model", ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		tmpl, err := getTemplate(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		o, err := d.Stores.Overlay.Get(ctx, overlay.KindModel, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		return &overlayOut{Body: buildOverlayView(tmpl, o)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "model_overlay_put", Method: "PUT", Path: "/models/by-id/{id}/overlay",
		Summary: "Set (replace) this model's overlay patch",
		Tags:    []string{"models"}, Middlewares: protect, Errors: []int{400, 401, 403, 404, 500},
	}, func(ctx context.Context, in *overlayPutInput) (*overlayOut, error) {
		if err := d.Authz.Authorize(ctx, "models.overlay.update", authz.Resource{Kind: "model", ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		tmpl, err := getTemplate(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		// Overlays change the model's effective state — gate on the same
		// governance rule as a direct edit.
		if err := settings.Governs(d.Catalog, settings.OpEdit, "model", string(tmpl.Meta.Owner.Kind)); err != nil {
			return nil, huma.Error403Forbidden(err.Error())
		}
		o := &overlay.Overlay{Kind: overlay.KindModel, ResourceID: in.ID, Patch: in.Body.Patch}
		if err := o.Validate(); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		// Reject patches that don't merge into a valid row NOW — the
		// quarantine path is for templates changing under a once-valid
		// patch, not for saving known-bad input.
		if _, err := overlay.EffectiveModel(tmpl, o); err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if err := d.Stores.Overlay.Upsert(ctx, o); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		stored, err := d.Stores.Overlay.Get(ctx, overlay.KindModel, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("stored but could not read back: " + err.Error())
		}
		return &overlayOut{Body: buildOverlayView(tmpl, stored)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "model_overlay_delete", Method: "DELETE", Path: "/models/by-id/{id}/overlay",
		Summary: "Delete this model's overlay (factory reset to the pristine template)",
		Tags:    []string{"models"}, Middlewares: protect, Errors: []int{401, 403, 404, 500},
	}, func(ctx context.Context, in *overlayIDInput) (*overlayDeleteOut, error) {
		if err := d.Authz.Authorize(ctx, "models.overlay.delete", authz.Resource{Kind: "model", ID: in.ID}); err != nil {
			return nil, mapAuthzErr(err)
		}
		tmpl, err := getTemplate(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		if err := settings.Governs(d.Catalog, settings.OpEdit, "model", string(tmpl.Meta.Owner.Kind)); err != nil {
			return nil, huma.Error403Forbidden(err.Error())
		}
		if err := d.Stores.Overlay.Delete(ctx, overlay.KindModel, in.ID); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &overlayDeleteOut{}
		out.Body.Deleted = true
		return out, nil
	})
}
