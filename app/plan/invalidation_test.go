package plan

import (
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/serviceaccount"
)

// ladder is two teams over two pools: team one (projects one and two) links pools A and B, team two links pool B only.
type ladder struct {
	*basic
	sa2, sa3     *serviceaccount.ServiceAccount
	key2, key3   *key.Key
	poolA, poolB *pool.Pool
	teamPol      *policy.Policy
	tierA        *policy.Policy
	h            *harness
}

func newLadder(t *testing.T) *ladder {
	b := newBasic()
	w := b.w
	l := &ladder{basic: b}
	l.sa2 = w.sa(w.project(b.team))
	l.key2 = w.saKey(l.sa2)
	team2 := w.team()
	l.sa3 = w.sa(w.project(team2))
	l.key3 = w.saKey(l.sa3)
	l.poolA = w.pool(b.hostA, b.a1, b.a2)
	l.poolB = w.pool(b.hostB, b.b1)
	rl := w.rateLimit(rule(ratelimit.MeterRequests, 100, time.Minute))
	l.teamPol = w.policy("team-one", policy.Spec{PoolIDs: []string{l.poolA.Meta.ID, l.poolB.Meta.ID}, RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	w.bindPolicy(l.teamPol, subject(policybinding.SubjectTeam, b.team.Meta.ID))
	team2Pol := w.policy("team-two", policy.Spec{PoolIDs: []string{l.poolB.Meta.ID}})
	w.bindPolicy(team2Pol, subject(policybinding.SubjectTeam, team2.Meta.ID))
	l.tierA = w.policy("tier-a", policy.Spec{})
	w.bindPolicy(l.tierA, subject(policybinding.SubjectAccount, b.acctA.Meta.ID))
	l.h = w.start(t)
	return l
}

// snapshot holds the published plan pointers the test compares after a write.
type snapshot struct {
	caller [3]*CallerPlan
	key    [3]*KeyPlan
	slot   map[string]*KeySlot
	pool   map[string]*PoolPlan
}

func (l *ladder) snap() snapshot {
	s := snapshot{slot: map[string]*KeySlot{}, pool: map[string]*PoolPlan{}}
	for i, k := range []*key.Key{l.key, l.key2, l.key3} {
		s.key[i] = get[KeyPlan](l.h, k.Meta.ID)
		s.caller[i] = s.key[i].Caller
	}
	for _, c := range []*hostkey.HostKey{l.a1, l.a2, l.b1} {
		s.slot[c.Meta.ID] = get[KeySlot](l.h, c.Meta.ID)
	}
	for _, p := range []*pool.Pool{l.poolA, l.poolB} {
		s.pool[p.Meta.ID] = get[PoolPlan](l.h, p.Meta.ID)
	}
	return s
}

// rebuilt reports, per principal (team one's two, team two's one), whether its caller plan and key plan were replaced.
func (l *ladder) rebuilt(t *testing.T, before snapshot, want [3]bool) snapshot {
	t.Helper()
	after := l.snap()
	for i := range want {
		if got := after.caller[i] != before.caller[i]; got != want[i] {
			t.Errorf("principal %d caller plan rebuilt = %v, want %v", i, got, want[i])
		}
		if got := after.key[i] != before.key[i]; got != want[i] {
			t.Errorf("principal %d key plan re-pointed = %v, want %v", i, got, want[i])
		}
	}
	return after
}

func sameSlots(t *testing.T, before, after snapshot, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if before.slot[id] != after.slot[id] {
			t.Errorf("credential plan %s rebuilt", id)
		}
	}
}

func TestInvalidation_CallerPolicyEditRebuildsItsTeamOnly(t *testing.T) {
	l := newLadder(t)
	before := l.snap()
	edit := clone(l.teamPol)
	edit.Spec.Models = []string{"acme"}
	apply(l.h, edit)
	after := l.rebuilt(t, before, [3]bool{true, true, false})
	sameSlots(t, before, after, l.a1.Meta.ID, l.a2.Meta.ID, l.b1.Meta.ID)
	if after.caller[0].Counters[0].ID != before.caller[0].Counters[0].ID {
		t.Error("adding a model to the policy restarted its rate-limit counter")
	}
}

func TestInvalidation_BindingChangeRebuildsItsSubjectOnly(t *testing.T) {
	l := newLadder(t)
	before := l.snap()
	p := l.w.policy("project-one", policy.Spec{Models: []string{"acme/m1"}})
	apply(l.h, p)
	apply(l.h, l.w.bindPolicy(p, subject(policybinding.SubjectProject, l.proj.Meta.ID)))
	l.rebuilt(t, before, [3]bool{true, false, false})
}

func TestInvalidation_PoolReorderKeepsRotationAndRebuildsItsCallers(t *testing.T) {
	l := newLadder(t)
	before := l.snap()
	re := clone(l.poolA)
	re.Spec.CredentialIDs = []string{l.a2.Meta.ID, l.a1.Meta.ID}
	apply(l.h, re)
	after := l.rebuilt(t, before, [3]bool{true, true, false})
	a, b := after.pool[l.poolA.Meta.ID], before.pool[l.poolA.Meta.ID]
	if a == b || a.Rotation != b.Rotation {
		t.Fatalf("pool rebuilt=%v, rotation kept=%v; want rebuilt with its rotation", a != b, a.Rotation == b.Rotation)
	}
	if after.pool[l.poolB.Meta.ID] != before.pool[l.poolB.Meta.ID] {
		t.Error("an unrelated pool was rebuilt")
	}
	sameSlots(t, before, after, l.a1.Meta.ID, l.a2.Meta.ID, l.b1.Meta.ID)
	wantHops(t, l.h.route(l.key, "m1"), ids(l.a2, l.a1), ids(l.b1))
}

func TestInvalidation_CredentialRotationSwapsTheBreakerOnly(t *testing.T) {
	l := newLadder(t)
	tier := clone(l.tierA)
	rl := l.w.rateLimit(rule(ratelimit.MeterTokens, 10000, time.Minute))
	apply(l.h, rl)
	tier.Spec.RateLimits = []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}
	apply(l.h, tier)
	before := l.snap()
	counters := l.h.route(l.key, "m1").Hops[0].Counters[0]

	rot := clone(l.a1)
	rotate(rot, "sk-rotated")
	apply(l.h, rot)
	after := l.rebuilt(t, before, [3]bool{true, true, false})
	s1, s0 := after.slot[l.a1.Meta.ID], before.slot[l.a1.Meta.ID]
	if s1 == s0 || s1.Breaker == s0.Breaker {
		t.Fatal("rotation kept the old breaker")
	}
	sameSlots(t, before, after, l.a2.Meta.ID, l.b1.Meta.ID)
	if after.pool[l.poolB.Meta.ID] != before.pool[l.poolB.Meta.ID] {
		t.Error("a pool without the credential was rebuilt")
	}
	got := l.h.route(l.key, "m1").Hops[0].Counters[0]
	if len(got) != len(counters) {
		t.Fatalf("counters %d after rotation, %d before", len(got), len(counters))
	}
	for i := range got {
		if got[i].ID != counters[i].ID {
			t.Errorf("counter %d moved on rotation: %s → %s", i, counters[i].ID, got[i].ID)
		}
	}
}

func TestInvalidation_LearnedOrgIDTouchesNothing(t *testing.T) {
	l := newLadder(t)
	before := l.snap()
	ap := get[AccountPlan](l.h, l.acctA.Meta.ID)
	learned := clone(l.acctA)
	learned.Spec.OrgID = "org-123"
	apply(l.h, learned)
	if get[AccountPlan](l.h, l.acctA.Meta.ID) != ap {
		t.Fatal("a learned org id rebuilt the account plan")
	}
	after := l.rebuilt(t, before, [3]bool{false, false, false})
	sameSlots(t, before, after, l.a1.Meta.ID, l.a2.Meta.ID, l.b1.Meta.ID)
}

func TestInvalidation_AccountTierEditRebuildsThatAccountsReach(t *testing.T) {
	l := newLadder(t)
	before := l.snap()
	tier := clone(l.tierA)
	tier.Spec.Models = []string{"acme/m1"}
	apply(l.h, tier)
	after := l.rebuilt(t, before, [3]bool{true, true, false})
	if after.slot[l.a1.Meta.ID] == before.slot[l.a1.Meta.ID] || after.slot[l.a2.Meta.ID] == before.slot[l.a2.Meta.ID] {
		t.Error("credential plans of the account were not rebuilt")
	}
	sameSlots(t, before, after, l.b1.Meta.ID)
	if after.pool[l.poolB.Meta.ID] != before.pool[l.poolB.Meta.ID] {
		t.Error("a pool of another account was rebuilt")
	}
}

func TestInvalidation_AccountMoveRepointsCooldown(t *testing.T) {
	l := newLadder(t)
	other := &account.Account{Meta: md("acct-other", system), Spec: account.Spec{HostID: l.hostA.Meta.ID}}
	apply(l.h, other)
	moved := clone(l.a2)
	moved.Spec.AccountID = other.Meta.ID
	apply(l.h, moved)
	s1, s2 := get[KeySlot](l.h, l.a1.Meta.ID), get[KeySlot](l.h, l.a2.Meta.ID)
	if s1.Account.Cooldown == s2.Account.Cooldown || s2.Account.Account.Meta.ID != other.Meta.ID {
		t.Fatal("a credential moved to another account kept the old account's cooldown")
	}
	del(l.h, KindAccount, other.Meta.ID)
	if get[KeySlot](l.h, l.a2.Meta.ID) != nil {
		t.Fatal("a credential of a deleted account still serves")
	}
	wantHops(t, l.h.route(l.key, "m1"), ids(l.a1), ids(l.b1))
}

func TestInvalidation_CatalogWriteRebuildsEveryCaller(t *testing.T) {
	l := newLadder(t)
	before := l.snap()
	apply(l.h, l.w.model(l.prov, "m9"))
	after := l.rebuilt(t, before, [3]bool{true, true, true})
	sameSlots(t, before, after, l.a1.Meta.ID, l.a2.Meta.ID, l.b1.Meta.ID)
	if after.caller[0].Routes == before.caller[0].Routes {
		t.Fatal("a catalog write kept the route table")
	}
}
