//go:build integration

package inference

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/routing"
	sdkcatalog "github.com/wyolet/relay/sdk/catalog"
)

// A policy-less key's catalog names exactly the slugs /v1/models lists for it, and each user's shared pool is its own cache scope.
func TestIntegration_CatalogPolicylessMatchesListedModels(t *testing.T) {
	cat, sfx := policylessCatalog(t, true)
	d := Deps{Catalog: cat, Resolver: routing.New(cat)}
	cache := newCatalogCache()
	userID := meta.NewID()
	pr := &Principal{CredentialKind: CredentialKey, CredentialID: meta.NewID(), UserID: userID}
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, pr)

	entry, err := scopedCatalog(ctx, d, cache)
	if err != nil {
		t.Fatalf("scopedCatalog: %v", err)
	}
	var doc sdkcatalog.Catalog
	if err := json.Unmarshal(entry.json, &doc); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range catalogSlugs(&doc) {
		if strings.HasSuffix(s, sfx) {
			got = append(got, s)
		}
	}
	var want []string
	for _, id := range listedIDs(t, d, pr) {
		if strings.HasSuffix(id, sfx) {
			want = append(want, id)
		}
	}
	if len(want) == 0 || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("catalog slugs = %v, /v1/models ids = %v", got, want)
	}
	if _, ok := cache.entries["policyless|"+userID]; !ok {
		t.Errorf("cache scopes = %v, want policyless|<userID>", cache.entries)
	}
}
