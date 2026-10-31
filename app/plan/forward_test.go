package plan

import (
	"testing"

	"github.com/wyolet/relay/pkg/engine"
)

func TestNotifyRefMapsCatalogKinds(t *testing.T) {
	for _, tc := range []struct {
		kind, id string
		want     engine.Ref
		ok       bool
	}{
		{"hostkey", "c1", engine.Ref{Kind: KindCredential, ID: "c1"}, true},
		{"relaykey", "k1", engine.Ref{Kind: KindKey, ID: "k1"}, true},
		{"hostbinding", "b1", engine.Ref{Kind: KindBinding, ID: "b1"}, true},
		{"overlay", "model|m1", engine.Ref{Kind: KindOverlay, ID: OverlayID("model", "m1")}, true},
		{"overlay", "broken", engine.Ref{}, false},
		{"pool", "p1", engine.Ref{Kind: KindPool, ID: "p1"}, true},
		{"user", "u1", engine.Ref{Kind: KindUser, ID: "u1"}, true},
		{"settings", "governance:policy", engine.Ref{}, false},
		{"role", "r1", engine.Ref{}, false},
	} {
		got, ok := NotifyRef(tc.kind, tc.id)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NotifyRef(%q, %q) = %v, %v; want %v, %v", tc.kind, tc.id, got, ok, tc.want, tc.ok)
		}
	}
	// Every input kind the plans register must be reachable from some notification.
	for _, k := range []engine.Kind{KindProvider, KindHost, KindModel, KindPricing, KindPolicy, KindPolicyBinding,
		KindRateLimit, KindBudget, KindAccount, KindTeam, KindProject, KindServiceAccount} {
		if got, ok := NotifyRef(string(k), "x"); !ok || got.Kind != k {
			t.Errorf("kind %s is not forwarded", k)
		}
	}
}
