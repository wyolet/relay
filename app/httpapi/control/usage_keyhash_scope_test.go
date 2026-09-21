package control

import (
	"context"
	"slices"
	"testing"

	"github.com/wyolet/relay/app/actor"
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
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/ids"
)

// Events written before the principal and project columns existed carry only
// a key hash. TestScopeOfCoversLegacyEventsByOwnKeyHash pins that a caller
// still reads their own history: the hashes come off their own key rows in
// the snapshot — current and pre-rotation both — and no other principal's.
func TestScopeOfCoversLegacyEventsByOwnKeyHash(t *testing.T) {
	yes := true
	k := &key.Key{Meta: meta.Metadata{ID: ids.New(), Name: "alice-key",
		Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"}}}
	k.Spec.Enabled = &yes
	k.Spec.KeyHash = "hash-alice-now"
	k.Spec.PreviousKeyHash = "hash-alice-old"
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: "u-alice"}

	cat := appcatalog.New(
		tokenList[provider.Provider]{}, tokenList[host.Host]{}, tokenList[policy.Policy]{},
		tokenList[model.Model]{}, tokenList[hostkey.HostKey]{}, tokenList[ratelimit.RateLimit]{},
		tokenList[key.Key]{k}, tokenList[pricing.Pricing]{}, tokenList[binding.Binding]{},
	)
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	ctx := actor.WithActor(context.Background(), scopeActors["alice"])
	sc, err := scopeOf(ctx, testRBAC(), cat, "usage")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hash-alice-now", "hash-alice-old"} {
		if !slices.Contains(sc.keyHashes, want) {
			t.Fatalf("keyHashes = %v, want %s in it", sc.keyHashes, want)
		}
	}
	if !sc.allows(usagelog.Event{RequestID: "r-1", RelayKeyHash: "hash-alice-old"}) {
		t.Fatalf("scope %+v rejects the caller's own pre-upgrade event", sc)
	}
	if sc.allows(usagelog.Event{RequestID: "r-2", RelayKeyHash: "hash-bob"}) {
		t.Fatalf("scope %+v admits a foreign key hash", sc)
	}

	var q usagelog.EventQuery
	if !scopeEventQuery(&q, sc) {
		t.Fatal("scopeEventQuery refused a scope that matches events")
	}
	if len(q.ScopeRelayKeyHash) != 2 {
		t.Fatalf("query hashes = %v, want both of the caller's", q.ScopeRelayKeyHash)
	}
}
