//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/wyolet/relay/app/seed"
)

const detachCatalog = `apiVersion: relay.wyolet.dev/v1alpha2
kind: Provider
metadata: {name: acme}
spec: {homepageURL: "https://acme.test"}
---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Host
metadata: {name: acme}
spec: {baseURL: "https://api.acme.test"}
`

const detachTier = `---
apiVersion: relay.wyolet.dev/v1alpha2
kind: Policy
metadata: {name: acme-tier-2, displayName: Tier 2, owner: {kind: host, name: acme}}
spec: {}
`

// A tier a catalog release dropped becomes the operator's: the default
// governance that refuses deleting catalog rows lets it go, an apply of its
// document neither re-attaches it nor reports drift, and a PUT cannot clear
// the mark.
func TestIntegration_DetachedCatalogRowIsTheOperators(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	pool := testPool(t, st.dsn)

	seedTree := func(docs string, detach bool) {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "catalog.yaml"), []byte(docs), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Run(ctx, seed.Options{Pool: pool, YAMLDir: dir, CatalogKindsOnly: true, DetachDropped: detach}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seedTree(detachCatalog+detachTier, false)

	read := func() map[string]any {
		t.Helper()
		code, body := st.adminDo(http.MethodGet, "/api/policies/acme-tier-2", "")
		if code != http.StatusOK {
			t.Fatalf("GET policy = %d: %s", code, body)
		}
		var row map[string]any
		if err := json.Unmarshal(body, &row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	md := read()["metadata"].(map[string]any)
	id := md["id"].(string)
	if _, ok := md["catalogDetachedAt"]; ok {
		t.Fatalf("a shipped tier reads as detached: %v", md)
	}
	if code, body := st.adminDo(http.MethodDelete, "/api/policies/by-id/"+id, ""); code != http.StatusForbidden {
		t.Fatalf("DELETE of a catalog tier = %d (%s), want 403 from the default governance", code, body)
	}

	seedTree(detachCatalog, true)
	detachedAt, ok := read()["metadata"].(map[string]any)["catalogDetachedAt"].(string)
	if !ok || detachedAt == "" {
		t.Fatalf("dropped tier has no catalogDetachedAt: %v", read()["metadata"])
	}

	code, plan, raw := st.applyBundle(detachCatalog+detachTier, "")
	if code != http.StatusOK || plan.action("Policy", "acme-tier-2") != "unchanged" {
		t.Fatalf("apply = %d, want the detached tier unchanged:\n%s", code, raw)
	}
	if got := read()["metadata"].(map[string]any)["catalogDetachedAt"]; got != detachedAt {
		t.Fatalf("apply moved catalogDetachedAt from %s to %v", detachedAt, got)
	}

	row := read()
	delete(row["metadata"].(map[string]any), "catalogDetachedAt")
	row["metadata"].(map[string]any)["displayName"] = "Tier 2 (ours)"
	blob, _ := json.Marshal(row)
	if code, body := st.adminDo(http.MethodPut, "/api/policies/by-id/"+id, string(blob)); code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", code, body)
	}
	if got := read()["metadata"].(map[string]any)["catalogDetachedAt"]; got != detachedAt {
		t.Fatalf("PUT moved catalogDetachedAt from %s to %v", detachedAt, got)
	}

	if code, body := st.adminDo(http.MethodDelete, "/api/policies/by-id/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("DELETE of a detached tier = %d (%s), want 204", code, body)
	}
}
