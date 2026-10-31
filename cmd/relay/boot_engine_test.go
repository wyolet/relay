package main

import (
	"testing"

	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/engine"
)

func TestPlanRefMapsNotifyKinds(t *testing.T) {
	for _, tc := range []struct {
		kind, id string
		want     engine.Ref
		ok       bool
	}{
		{"hostkey", "c1", engine.Ref{Kind: plan.KindCredential, ID: "c1"}, true},
		{"relaykey", "k1", engine.Ref{Kind: plan.KindKey, ID: "k1"}, true},
		{"hostbinding", "b1", engine.Ref{Kind: plan.KindBinding, ID: "b1"}, true},
		{"overlay", "model|m1", engine.Ref{Kind: plan.KindOverlay, ID: plan.OverlayID("model", "m1")}, true},
		{"overlay", "broken", engine.Ref{}, false},
		{"pool", "p1", engine.Ref{Kind: plan.KindPool, ID: "p1"}, true},
		{"user", "u1", engine.Ref{Kind: plan.KindUser, ID: "u1"}, true},
		{"settings", "governance:policy", engine.Ref{}, false},
		{"role", "r1", engine.Ref{}, false},
	} {
		got, ok := planRef(tc.kind, tc.id)
		if got != tc.want || ok != tc.ok {
			t.Errorf("planRef(%q, %q) = %v, %v; want %v, %v", tc.kind, tc.id, got, ok, tc.want, tc.ok)
		}
	}
	// Every input kind the plans register must be reachable from some notification.
	for _, k := range []engine.Kind{plan.KindProvider, plan.KindHost, plan.KindModel, plan.KindPricing, plan.KindPolicy, plan.KindPolicyBinding,
		plan.KindRateLimit, plan.KindBudget, plan.KindAccount, plan.KindTeam, plan.KindProject, plan.KindServiceAccount} {
		if got, ok := planRef(string(k), "x"); !ok || got.Kind != k {
			t.Errorf("kind %s is not forwarded", k)
		}
	}
}
