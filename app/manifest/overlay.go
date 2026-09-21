package manifest

import (
	"encoding/json"
	"fmt"

	"github.com/wyolet/relay/app/overlay"
)

// OverlayDTO is the wire form of a catalog overlay. Overlays carry no
// identity of their own — the row is keyed by (target kind, target row) —
// so metadata.name is the *name of the overlaid row*, not of the overlay.
type OverlayDTO struct {
	APIVersion string      `json:"apiVersion" yaml:"apiVersion"`
	Kind       string      `json:"kind"       yaml:"kind"`
	Metadata   WireMeta    `json:"metadata"   yaml:"metadata"`
	Spec       OverlaySpec `json:"spec"       yaml:"spec"`
}

type OverlaySpec struct {
	// Target is the overlaid kind. v1 supports "model" only.
	Target string `json:"target" yaml:"target"`
	// Patch is the sparse spec patch merged onto the template row.
	Patch map[string]any `json:"patch" yaml:"patch"`
}

// ToOverlay resolves the overlaid row's name to its id.
func ToOverlay(d OverlayDTO, idx Resolver) (*overlay.Overlay, error) {
	if d.Spec.Target != overlay.KindModel {
		return nil, fmt.Errorf("overlay: unsupported spec.target %q (want %q)", d.Spec.Target, overlay.KindModel)
	}
	id, ok := idx.ModelID(d.Metadata.Name)
	if !ok {
		return nil, fmt.Errorf("overlay: model %q not found", d.Metadata.Name)
	}
	patch, err := json.Marshal(d.Spec.Patch)
	if err != nil {
		return nil, fmt.Errorf("overlay: patch: %w", err)
	}
	o := &overlay.Overlay{Kind: overlay.KindModel, ResourceID: id, Patch: patch}
	return o, o.Validate()
}

// FromOverlay renders an overlay against the overlaid row's name.
func FromOverlay(o *overlay.Overlay, rev ReverseResolver) (OverlayDTO, error) {
	name, ok := rev.ModelName(o.ResourceID)
	if !ok {
		name = o.ResourceID
	}
	var patch map[string]any
	if err := json.Unmarshal(o.Patch, &patch); err != nil {
		return OverlayDTO{}, fmt.Errorf("overlay %s: patch: %w", o.ResourceID, err)
	}
	return OverlayDTO{
		APIVersion: APIVersion,
		Kind:       "Overlay",
		Metadata:   WireMeta{Name: name},
		Spec:       OverlaySpec{Target: o.Kind, Patch: patch},
	}, nil
}
