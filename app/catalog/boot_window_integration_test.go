//go:build integration

package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/provider"
)

// TestIntegration_WriteDuringBootWindowNotLost reproduces the audit finding
// "Listener LISTENs only after initial Reload — boot window loses writes"
// (audit 2026-07-04, audit-catalog.md, boot.go Hydrate + notify.go listen).
//
// Hydrate runs the initial Reload and only *constructs* the Listener;
// LISTEN catalog_events is issued later inside listener.Run. A write
// committed in that window (replica B applies an admin write while replica
// A boots) fires a NOTIFY nobody hears — the row is invisible to this
// process until an unrelated event for the same row or a manual /reload.
//
// The window is made deterministic here: the write commits after Hydrate
// returns (Reload done) and before Run is started (no LISTEN yet). No
// sleeps race against the bug — the NOTIFY provably precedes the LISTEN.
// Correct behavior (LISTEN attached before the first snapshot is built)
// makes the write visible shortly after Run starts.
func TestIntegration_WriteDuringBootWindowNotLost(t *testing.T) {
	t.Skip("audit 2026-07-04: LISTEN-after-load boot window loses writes — known-broken, unskip with the fix")
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

	// The boot window: initial Reload complete, LISTEN not yet issued.
	p := &provider.Provider{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "boot-window-prov",
			Owner: meta.Owner{Kind: meta.OwnerSystem},
		},
	}
	if err := stores.Provider.Upsert(ctx, p); err != nil {
		t.Fatalf("upsert during boot window: %v", err)
	}

	listenerCtx, listenerCancel := context.WithCancel(ctx)
	defer listenerCancel()
	go func() { _ = listener.Run(listenerCtx) }()

	// Generous budget: LISTEN attach + several 1s debounce flush cycles.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cat.Current().Provider(p.Meta.ID); ok {
			return // write survived the boot window
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("provider %s committed during the boot window (post-Reload, pre-LISTEN) never reached the snapshot: the NOTIFY fired before LISTEN attached and the write is lost until a manual reload", p.Meta.ID)
}
