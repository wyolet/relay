// Package overlay is the domain layer for catalog overlays: user-owned
// sparse spec patches applied on top of pristine catalog TEMPLATE rows
// to produce the EFFECTIVE rows the snapshot serves.
//
// Why this exists: catalog-managed resources are re-seeded from the
// upstream catalog, and a direct edit to such a row is clobbered by the
// next re-seed (or, worse, blocks it). An overlay stores ONLY the fields
// the user changed; the merge happens at catalog load time (app/catalog
// snapshot build / reconcile), never in storage — so re-seeding replaces
// templates and is completely overlay-unaware, while un-overridden
// fields keep tracking the upstream catalog. Factory reset = delete the
// overlay row. Full design + decided trade-offs: design/overlays.md.
//
// Deliberately out of scope (v1): kinds other than "model"; diff-on-write
// against generic CRUD PUT (the overlay is an explicitly user-managed
// patch document); per-field replace-mode escalation on union fields;
// metadata/identity patches (a name-overriding overlay — a "clone" — is
// a designed future, not built); patching `enabled` (membership stays a
// sanctioned direct edit so the snapshot's enabled-filter semantics
// never depend on a merge).
package overlay

import (
	"encoding/json"
	"fmt"
	"time"
)

// KindModel is the only overlayable kind in v1. The schema and store are
// kind-generic so new kinds are data, not migrations.
const KindModel = "model"

// Overlay is one user-owned sparse patch targeting a template row.
// Patch is a JSON object of SPEC fields only — top-level keys merge into
// the target's spec per the kind's Strategy (see merge.go). It is NOT a
// full resource: metadata is never patchable.
type Overlay struct {
	Kind       string          `json:"kind"`
	ResourceID string          `json:"resourceId"`
	Patch      json.RawMessage `json:"patch"`
	UpdatedAt  time.Time       `json:"updatedAt,omitempty"`
}

// forbiddenPatchKeys are spec fields an overlay may not touch.
// `enabled` controls snapshot membership: the enabled-filter runs on
// templates before any merge, so allowing it here would create rows
// that are half in, half out of the snapshot.
var forbiddenPatchKeys = map[string]struct{}{
	"enabled": {},
}

// Validate enforces the overlay's intra-row rules. Post-merge validation
// (the effective row must pass the target kind's Validate) is the
// caller's job — it needs the template.
func (o *Overlay) Validate() error {
	if o.Kind != KindModel {
		return fmt.Errorf("overlay: unsupported kind %q (v1 supports %q)", o.Kind, KindModel)
	}
	if o.ResourceID == "" {
		return fmt.Errorf("overlay: resourceId is required")
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(o.Patch, &patch); err != nil {
		return fmt.Errorf("overlay: patch must be a JSON object: %w", err)
	}
	if len(patch) == 0 {
		return fmt.Errorf("overlay: patch is empty — delete the overlay instead")
	}
	for k := range patch {
		if _, bad := forbiddenPatchKeys[k]; bad {
			return fmt.Errorf("overlay: field %q is not patchable via overlay — edit the resource directly", k)
		}
	}
	return nil
}

// Key returns the snapshot index key for this overlay's target.
func (o *Overlay) Key() string { return TargetKey(o.Kind, o.ResourceID) }

// TargetKey builds the kind|resourceID composite key used by the
// snapshot index and the NOTIFY payload.
func TargetKey(kind, resourceID string) string { return kind + "|" + resourceID }
