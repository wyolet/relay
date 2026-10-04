package apply

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/overlay"
)

// planOverlays diffs the model overlays. Overlays carry no metadata of their
// own — they are keyed by (kind, target row) and patch a shared catalog row,
// so their entries carry a system owner — they have no dirty flag, are never
// pruned, and authorize under the model verbs the overlay endpoints already use.
func (b *builder) planOverlays(docs []*manifest.OverlayDTO) error {
	if len(docs) == 0 {
		return nil
	}
	if b.opts.Stores.Overlay == nil {
		return fmt.Errorf("apply: overlay store not wired")
	}
	existing := make(map[string]*overlay.Overlay, len(b.rows.Overlays))
	for _, o := range b.rows.Overlays {
		existing[o.Key()] = o
	}
	route := KindRoutes["Overlay"]
	for _, d := range docs {
		o, err := manifest.ToOverlay(*d, b.idx)
		if err != nil {
			return fmt.Errorf("apply: Overlay %q: %w", d.Metadata.Name, err)
		}
		e := Entry{
			Kind: "Overlay", Name: d.Metadata.Name, ID: o.ResourceID,
			plural: route.Plural, owner: meta.Owner{Kind: meta.OwnerSystem},
		}
		prev, found := existing[o.Key()]
		switch {
		case !found:
			e.Action = ActionCreate
		case string(prev.Patch) == string(o.Patch):
			e.Action = ActionUnchanged
		default:
			e.Action = ActionUpdate
			e.ChangedFields = []string{"spec.patch"}
		}
		if e.Action == ActionCreate || e.Action == ActionUpdate {
			row := o
			e.write = func(ctx context.Context) error { return b.opts.Stores.Overlay.Upsert(ctx, row) }
		}
		b.entries = append(b.entries, e)
	}
	return nil
}
