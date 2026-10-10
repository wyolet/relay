package batch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/routing"
)

// The submission already resolved a policy; if it has since left the
// snapshot the item fails. Running it policy-less would execute it under
// rules and a key pool the caller never had.
func TestRun_MissingPolicyFailsTheItem(t *testing.T) {
	rn, _, _ := runnerFixture(t)
	_, _, err := rn.Run(context.Background(), "item-1", fixtureKeyHash, meta.NewID(), TokenClaims{},
		keyAttr(), adapters.OpenAI, []byte(`{"model":"test-model"}`))
	if !errors.Is(err, ErrPolicyUnavailable) {
		t.Fatalf("err = %v, want ErrPolicyUnavailable", err)
	}
}

// The runner routes against the snapshot it read at entry, not
// whatever the resolver's catalog holds when Resolve runs — one item must not
// straddle two catalog views.
func TestRun_PinsItsSnapshot(t *testing.T) {
	// A resolver over an empty catalog: without pinning, resolution reads it
	// and cannot find the model.
	empty := catalogtest.Catalog{}.Load(t)
	rn, _, policyID := runnerFixture(t)
	rn.Resolver = routing.New(empty)

	_, _, err := rn.Run(context.Background(), "item-1", fixtureKeyHash, policyID, TokenClaims{},
		keyAttr(), adapters.OpenAI, []byte(`{"model":"test-model"}`))
	if err == nil {
		t.Fatal("expected the run to fail past routing on the unreachable upstream")
	}
	if strings.Contains(err.Error(), routing.ErrModelNotFound.Error()) {
		t.Fatalf("routing read the resolver's catalog instead of the pinned snapshot: %v", err)
	}
}
