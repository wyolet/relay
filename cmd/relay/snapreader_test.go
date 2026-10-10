package main

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

// The rate-limit rules a request is metered by must come from the snapshot
// the request was authenticated against, not from whatever reload landed
// while it was in flight.
func TestSnapReaderServesTheRequestSnapshot(t *testing.T) {
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "p", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	cat := catalogtest.Catalog{Policies: []*policy.Policy{pol}}.Load(t)
	pinned := cat.Current()
	r := catalogSnapReader{cat: cat}

	// A reload drops the policy; a request pinned to the older snapshot
	// still resolves it, a fresh caller does not.
	if err := cat.ApplyPolicyDelete(pol.Meta.ID); err != nil {
		t.Fatalf("delete policy: %v", err)
	}
	ctx := inference.WithSnapshot(context.Background(), pinned)
	if _, ok := r.Policy(ctx, pol.Meta.ID); !ok {
		t.Error("pinned snapshot lost the policy the request resolved against")
	}
	if _, ok := r.Policy(context.Background(), pol.Meta.ID); ok {
		t.Error("an unpinned read served a policy the current snapshot dropped")
	}
}
