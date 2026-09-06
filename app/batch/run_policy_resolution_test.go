package batch

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/routing"
)

// The submission already resolved a policy; if it has since left the
// snapshot the item fails. Running it policy-less would execute it under
// rules and a key pool the caller never had.
func TestRun_MissingPolicyFailsTheItem(t *testing.T) {
	rn, _, _ := runnerFixture(t)
	_, _, err := rn.Run(context.Background(), "item-1", "", meta.NewID(), "",
		Attribution{}, adapters.OpenAI, []byte(`{"model":"test-model"}`))
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
	empty := appcatalog.New(
		lister[provider.Provider]{}, lister[host.Host]{}, lister[policy.Policy]{},
		lister[model.Model]{}, lister[hostkey.HostKey]{}, lister[ratelimit.RateLimit]{},
		lister[key.Key]{}, lister[pricing.Pricing]{}, lister[binding.Binding]{},
	)
	if err := empty.Reload(t.Context()); err != nil {
		t.Fatalf("reload empty catalog: %v", err)
	}
	rn, _, policyID := runnerFixture(t)
	rn.Resolver = routing.New(empty)

	_, _, err := rn.Run(context.Background(), "item-1", "", policyID, "",
		Attribution{}, adapters.OpenAI, []byte(`{"model":"test-model"}`))
	if err == nil {
		t.Fatal("expected the run to fail past routing on the unreachable upstream")
	}
	if strings.Contains(err.Error(), routing.ErrModelNotFound.Error()) {
		t.Fatalf("routing read the resolver's catalog instead of the pinned snapshot: %v", err)
	}
}
