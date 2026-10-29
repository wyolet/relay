package policy

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// runRenameSuite pins that every counter a policy owns follows its id, so renaming the policy carries its rate-limit budget and key rotation over.
func runRenameSuite(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	t.Run("inbound_budget_survives_rename", func(t *testing.T) {
		f := newAdmitFixture(t, newStore(t), keypool.KeySelectionPrioritized, 1)
		f.in.TeamID = "" // anchor on the policy, not the team
		ctx := context.Background()
		if _, err := f.svc.Admit(ctx, f.in, f.acq); err != nil {
			t.Fatalf("first Admit: %v", err)
		}
		f.in.Policy.Meta.Name = "caller-renamed"
		if _, err := f.svc.Admit(ctx, f.in, f.acq); !errors.Is(err, pkgratelimit.ErrExceeded) {
			t.Fatalf("Admit after rename: err = %v, want ErrExceeded from the same budget", err)
		}
	})

	for _, algo := range []keypool.KeySelection{keypool.KeySelectionRoundRobin, keypool.KeySelectionLeastRecentlyUsed} {
		t.Run(string(algo)+"_rotation_survives_rename", func(t *testing.T) {
			f := newAdmitFixture(t, newStore(t), algo, 10)
			ctx := context.Background()
			adm, err := f.svc.Admit(ctx, f.in, f.acq)
			if err != nil || adm.Acq == nil || adm.Acq.Key != f.keys[0] {
				t.Fatalf("first Admit = %+v, %v; want the first key", adm.Acq, err)
			}
			f.acq.Policy.Meta.Name = "caller-renamed"
			adm, err = f.svc.Admit(ctx, f.in, f.acq)
			if err != nil || adm.Acq == nil || adm.Acq.Key != f.keys[1] {
				t.Fatalf("Admit after rename = %+v, %v; want the rotation to move on to the second key", adm.Acq, err)
			}
		})
	}
}

func TestRenameKeysMem(t *testing.T) {
	runRenameSuite(t, func(t *testing.T) kvtest.ScriptStore { return admitMem(t) })
}

func TestRuleKeys_IgnorePolicyName(t *testing.T) {
	rl := testRateLimit("rl-1", requestsRule())
	pol := fix("before")
	pol.Meta.ID = "pol-1"
	before := pol.ResolveRules(rl, "m-1")
	beforeScope := inboundScope(InboundInput{Policy: pol})
	beforePick, _ := pickScope(pol)

	pol.Meta.Name = "after"
	after := pol.ResolveRules(rl, "m-1")
	if before[0].Key != after[0].Key {
		t.Errorf("rule key %q -> %q on rename", before[0].Key, after[0].Key)
	}
	if got := inboundScope(InboundInput{Policy: pol}); got != beforeScope || got != "policy:pol-1" {
		t.Errorf("inbound scope %q -> %q on rename, want policy:pol-1", beforeScope, got)
	}
	if got, _ := pickScope(pol); got != beforePick || got != "pol-1" {
		t.Errorf("pick scope %q -> %q on rename, want pol-1", beforePick, got)
	}
}
