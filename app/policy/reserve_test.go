package policy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// countingKV records every script call so a test can pin how many kv round
// trips one request costs.
type countingKV struct {
	*kv.Mem
	mu    sync.Mutex
	names []string
	keys  [][]string
}

func newCountingKV() *countingKV {
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	return &countingKV{Mem: mem}
}

func (c *countingKV) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	c.mu.Lock()
	c.names = append(c.names, name)
	c.keys = append(c.keys, append([]string(nil), keys...))
	c.mu.Unlock()
	return c.Mem.RunScript(ctx, name, script, keys, args...)
}

func (c *countingKV) reserveCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, name := range c.names {
		if name == "limit.reserve" {
			n++
		}
	}
	return n
}

func (c *countingKV) lastKeys() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keys[len(c.keys)-1]
}

// reserveSnap answers the two lookups rulesFor makes.
type reserveSnap struct {
	pol *Policy
	rl  *appratelimit.RateLimit
}

func (s reserveSnap) Policy(string) (*Policy, bool) { return s.pol, s.pol != nil }
func (s reserveSnap) RateLimit(id string) (*appratelimit.RateLimit, bool) {
	if s.rl == nil || s.rl.Meta.ID != id {
		return nil, false
	}
	return s.rl, true
}

func reserveFixture(t testing.TB, rules ...appratelimit.Rule) (*Service, *countingKV, *Policy) {
	t.Helper()
	pol := fix("prod-policy")
	pol.Meta.ID = "pol-1"
	var rl *appratelimit.RateLimit
	if len(rules) > 0 {
		rl = &appratelimit.RateLimit{}
		rl.Meta.ID = "rl-1"
		rl.Meta.Name = "rl-1"
		rl.Spec.Rules = rules
		pol.Spec.RateLimitID = rl.Meta.ID
	}
	store := newCountingKV()
	t.Cleanup(func() { _ = store.Close() })
	return NewService(reserveSnap{pol: pol, rl: rl}, nil, pkgratelimit.New(store, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)), store, pol
}

// TestReserveInbound_TokenWithNoRules pins the hot-path invariant: a token
// request still costs exactly one script even when the policy caps nothing,
// because the revocation check has to ride that call.
func TestReserveInbound_TokenWithNoRules(t *testing.T) {
	svc, store, pol := reserveFixture(t)

	res, err := svc.ReserveInbound(context.Background(), InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-1",
	})
	if err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if res == nil {
		t.Fatal("reservation is nil, want one covering the revocation check")
	}
	if got := store.reserveCalls(); got != 1 {
		t.Fatalf("reserve scripts = %d, want exactly 1", got)
	}
	want := "limit:{team:team-1}:jti:jti-1"
	if keys := store.lastKeys(); len(keys) != 1 || keys[0] != want {
		t.Fatalf("script keys = %v, want [%s]", keys, want)
	}
}

// TestReserveInbound_KeyWithNoRules is the other half of the invariant: a key
// request with nothing to meter makes no kv call at all.
func TestReserveInbound_KeyWithNoRules(t *testing.T) {
	svc, store, pol := reserveFixture(t)

	res, err := svc.ReserveInbound(context.Background(), InboundInput{Policy: pol})
	if err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if res != nil {
		t.Fatalf("reservation = %+v, want nil (nothing to reserve)", res)
	}
	if got := store.reserveCalls(); got != 0 {
		t.Fatalf("reserve scripts = %d, want 0", got)
	}
}

// TestReserveInbound_OneScriptWithRules keeps the rate-limited path at one
// call too, with the denylist key alongside the rule keys.
func TestReserveInbound_OneScriptWithRules(t *testing.T) {
	svc, store, pol := reserveFixture(t, appratelimit.Rule{
		Meter: appratelimit.MeterRequests, Amount: 10, Window: appratelimit.Window(time.Minute),
	})

	if _, err := svc.ReserveInbound(context.Background(), InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-1",
	}); err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if got := store.reserveCalls(); got != 1 {
		t.Fatalf("reserve scripts = %d, want exactly 1", got)
	}
	keys := store.lastKeys()
	if len(keys) != 2 {
		t.Fatalf("script keys = %v, want the rule key plus the denylist key", keys)
	}
	for _, k := range keys {
		if !strings.HasPrefix(k, "limit:{team:team-1}:") {
			t.Errorf("key %q is not under the team hash tag", k)
		}
	}
}

// TestReserveInbound_RevokedJTI proves revocation lands inside the Reserve
// call — before any upstream work — once the denylist entry exists.
func TestReserveInbound_RevokedJTI(t *testing.T) {
	svc, store, pol := reserveFixture(t)
	if err := store.Set(context.Background(), RevokedKey("team-1", "jti-1"), []byte("1"), time.Hour); err != nil {
		t.Fatalf("write denylist entry: %v", err)
	}

	_, err := svc.ReserveInbound(context.Background(), InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-1",
	})
	if !errors.Is(err, pkgratelimit.ErrRevoked) {
		t.Fatalf("err = %v, want ErrRevoked", err)
	}
	// A different token in the same team is unaffected.
	if _, err := svc.ReserveInbound(context.Background(), InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-2",
	}); err != nil {
		t.Fatalf("second token: %v", err)
	}
}

// TestReserveInbound_ScopeTag pins D26: a project-scoped caller anchors on
// its team, everyone else keeps the policy slug.
func TestReserveInbound_ScopeTag(t *testing.T) {
	rule := appratelimit.Rule{Meter: appratelimit.MeterRequests, Amount: 10, Window: appratelimit.Window(time.Minute)}

	for _, tc := range []struct {
		name   string
		teamID string
		want   string
	}{
		{name: "project request anchors on the team", teamID: "team-1", want: "limit:{team:team-1}:"},
		{name: "personal request keeps the policy slug", want: "limit:{prod-policy}:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, pol := reserveFixture(t, rule)
			if _, err := svc.ReserveInbound(context.Background(), InboundInput{Policy: pol, TeamID: tc.teamID}); err != nil {
				t.Fatalf("ReserveInbound: %v", err)
			}
			for _, k := range store.lastKeys() {
				if !strings.HasPrefix(k, tc.want) {
					t.Errorf("key %q, want prefix %q", k, tc.want)
				}
			}
		})
	}
}

// TestReserveInbound_RevocationRuleIsFirst pins the rule order: the token
// revocation check must reach the limiter ahead of any rate-limit rule, so
// a revoked token answers 401 rather than the 429 an over-limit rule
// evaluated first would produce.
func TestReserveInbound_RevocationRuleIsFirst(t *testing.T) {
	svc, store, pol := reserveFixture(t, appratelimit.Rule{
		Meter: appratelimit.MeterRequests, Amount: 10, Window: appratelimit.Window(time.Minute),
	})

	if _, err := svc.ReserveInbound(context.Background(), InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-1",
	}); err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	keys := store.lastKeys()
	if len(keys) != 2 {
		t.Fatalf("script keys = %v, want the denylist key plus the rate-limit rule key", keys)
	}
	want := RevokedKey("team-1", "jti-1")
	if keys[0] != want {
		t.Fatalf("first key = %q, want the revocation key %q first", keys[0], want)
	}
}

// TestReserveInbound_ThenCommit_IsOneScriptTotal is the other half of the
// no-commit invariant: not just Reserve, but Reserve+Commit together must
// cost exactly one kv round trip when revocation is the only rule — the
// commit-side script must be skipped, not merely a no-op RunScript call.
func TestReserveInbound_ThenCommit_IsOneScriptTotal(t *testing.T) {
	svc, store, pol := reserveFixture(t)

	res, err := svc.ReserveInbound(context.Background(), InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-1",
	})
	if err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if err := svc.CommitInbound(context.Background(), res, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("CommitInbound: %v", err)
	}
	if got := len(store.names); got != 1 {
		t.Fatalf("total script calls = %d (%v), want exactly 1 (Reserve only, Commit skipped)", got, store.names)
	}
}

func TestRevokedKeyFormat(t *testing.T) {
	if got := RevokedKey("019200aa", "019200bb"); got != "limit:{team:019200aa}:jti:019200bb" {
		t.Errorf("RevokedKey = %q", got)
	}
	if got := reserveScope("", "prod-policy"); got != "prod-policy" {
		t.Errorf("reserveScope without a team = %q, want the policy slug", got)
	}
}
