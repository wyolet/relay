//go:build integration

package catalog

import (
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/provider"
)

// Hydrate runs the initial Reload and only constructs the Listener; LISTEN
// is issued later inside Run. A write committed in that window fires a
// NOTIFY nobody hears, so it must still reach the snapshot once LISTEN
// attaches. The write provably precedes LISTEN: Run has not started yet.
func TestIntegration_WriteDuringBootWindowNotLost(t *testing.T) {
	pool, ctx, cancel := setupDB(t)
	defer cancel()

	opts := BootstrapOptions{Pool: pool}
	cat, stores, err := BootstrapStores(ctx, opts)
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}
	listener, err := cat.Hydrate(ctx, stores, opts)
	if err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	p := &provider.Provider{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "boot-window-prov",
			Owner: meta.Owner{Kind: meta.OwnerSystem},
		},
	}
	if err := stores.Provider.Upsert(ctx, p); err != nil {
		t.Fatalf("upsert during boot window: %v", err)
	}

	runListener(t, ctx, cat, listener)
	if _, ok := cat.Current().Provider(p.Meta.ID); !ok {
		t.Fatalf("provider %s committed before LISTEN attached is missing from the snapshot", p.Meta.ID)
	}
}
