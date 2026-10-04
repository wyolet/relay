package policy

// reserve_alloc_test.go pins the allocation cost of the inbound reservation —
// the other half of the per-request budget the credential middleware's gate
// covers. The numbers are the measured current cost, not a budget with
// headroom: a change that allocates more has to be seen and re-pinned
// deliberately, because this runs on every metered /v1/* call.
//
// The store is a stub that answers every script with a canned granted reply.
// A real kv.Mem would put its script emulator's own map and slice growth —
// an order of magnitude more allocations than the reservation makes, and
// unstable under the race detector — inside the number being pinned.

import (
	"context"
	"testing"

	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// grantingStore answers every script with the reply a granted reservation
// decodes from. The bytes are allocated once, at construction: what the gate
// measures is the caller's own work, not the store's.
type grantingStore struct {
	*kv.Mem
	reply []byte
}

func newGrantingStore(t *testing.T) *grantingStore {
	t.Helper()
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	// Every field of the reserve result is false or zero in a grant, and the
	// commit result is only logged.
	return &grantingStore{Mem: mem, reply: []byte(`{}`)}
}

func (s *grantingStore) RunScript(context.Context, string, string, []string, ...any) ([]byte, error) {
	return s.reply, nil
}

// allocFixture wires a Service whose limiter talks to the stub store.
func allocFixture(t *testing.T, rules ...appratelimit.Rule) (*Service, *Policy) {
	t.Helper()
	pol := fix("prod-policy")
	pol.Meta.ID = "pol-1"
	var rl *appratelimit.RateLimit
	if len(rules) > 0 {
		rl = testRateLimit("rl-1", rules...)
		pol.Spec.RateLimitID = rl.Meta.ID
	}
	return NewService(reserveSnap{pol: pol, rl: rl}, nil,
		pkgratelimit.New(newGrantingStore(t), discardLogger(), nil)), pol
}

func allocsForReserve(t *testing.T, svc *Service, in InboundInput) int {
	t.Helper()
	ctx := context.Background()
	// A reservation that errors would measure the rejection path instead of
	// the one this gate claims to pin.
	if _, err := svc.ReserveInbound(ctx, in); err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	return int(testing.AllocsPerRun(200, func() {
		res, err := svc.ReserveInbound(ctx, in)
		if err != nil {
			t.Fatalf("ReserveInbound: %v", err)
		}
		_ = res
	}))
}

// assertAllocs applies the pin. Without the race detector the count is exact
// and reproducible; with it, shadow-memory bookkeeping varies per run, so the
// gate falls back to a ceiling that still catches a real regression.
func assertAllocs(t *testing.T, label string, got, want int) {
	t.Helper()
	if !raceInstrumented {
		if got != want {
			t.Fatalf("%s allocates %d objects per request, pinned at %d — "+
				"re-pin deliberately if the change is intended", label, got, want)
		}
		return
	}
	if ceiling := want * 3 / 2; got > ceiling {
		t.Fatalf("%s allocates %d objects per request under -race, over the %d ceiling "+
			"around the pin of %d", label, got, ceiling, want)
	}
}

func allocGateRules() []appratelimit.Rule {
	w := appratelimit.Window(1 << 40)
	return []appratelimit.Rule{
		{Meter: appratelimit.MeterRequests, Amount: 1 << 40, Window: w, Strategy: appratelimit.StrategyFixedWindow},
		{Meter: appratelimit.MeterTokens, Amount: 1 << 40, Window: w, Strategy: appratelimit.StrategyFixedWindow},
		{Meter: appratelimit.MeterTokensInput, Amount: 1 << 40, Window: w, Strategy: appratelimit.StrategyFixedWindow},
	}
}

// TestReserveInbound_Allocations pins both halves of the reservation: a key
// request renders the policy's rules and makes one script call; a token
// request additionally prepends the revocation rule into a pre-sized slice,
// which is why the extra rule must not cost a regrow.
func TestReserveInbound_Allocations(t *testing.T) {
	for _, tc := range []struct {
		name string
		jti  string
		want int
	}{
		{name: "key", want: 54},
		{name: "token", jti: "jti-1", want: 59},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, pol := allocFixture(t, allocGateRules()...)
			in := InboundInput{
				Policy: pol, TeamID: "team-1", TokenJTI: tc.jti,
				ProviderSlug: "prov", ModelSlug: "model", HostSlug: "host",
			}
			assertAllocs(t, tc.name+" reservation", allocsForReserve(t, svc, in), tc.want)
		})
	}
}

// A token whose policy caps nothing still pays for the one script call the
// revocation check rides on, and nothing more.
func TestReserveInbound_AllocationsWithNoRules(t *testing.T) {
	const want = 21

	svc, pol := allocFixture(t)
	in := InboundInput{Policy: pol, TeamID: "team-1", TokenJTI: "jti-1"}
	assertAllocs(t, "zero-rule token reservation", allocsForReserve(t, svc, in), want)
}

// A key request with nothing to meter reaches no kv call at all, so it must
// allocate nothing.
func TestReserveInbound_AllocationsWithNothingToDo(t *testing.T) {
	svc, pol := allocFixture(t)
	if got := allocsForReserve(t, svc, InboundInput{Policy: pol}); got != 0 {
		t.Fatalf("a reservation with nothing to check allocates %d objects, want 0", got)
	}
}

// The commit half of the budget: returning a reservation must not allocate
// more than the script call it makes.
func TestCommitInbound_Allocations(t *testing.T) {
	const want = 38

	svc, pol := allocFixture(t, allocGateRules()...)
	ctx := context.Background()
	in := InboundInput{Policy: pol, TeamID: "team-1", ProviderSlug: "prov", ModelSlug: "model", HostSlug: "host"}
	obs := pkgratelimit.Observations{Tokens: map[string]int64{"input": 10, "output": 5}}

	both := int(testing.AllocsPerRun(200, func() {
		res, err := svc.ReserveInbound(ctx, in)
		if err != nil {
			t.Fatalf("ReserveInbound: %v", err)
		}
		if err := svc.CommitInbound(ctx, res, obs); err != nil {
			t.Fatalf("CommitInbound: %v", err)
		}
	}))
	assertAllocs(t, "the commit half", both-allocsForReserve(t, svc, in), want)
}
