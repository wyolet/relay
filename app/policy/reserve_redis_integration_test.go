//go:build integration

// reserve_redis_integration_test.go runs the inbound reservation against a
// real Redis. The one-script-per-request budget is a claim about a network
// round trip, and the in-memory store cannot falsify it: kv.Mem's emulator
// would happily serve a rule set that a real EVAL rejects for spanning slots.
// Run with: make test-integration.
package policy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	appratelimit "github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

func startRedis(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:        "redis:7-alpine",
			ExposedPorts: []string{"6379/tcp"},
			WaitingFor:   wait.ForLog("Ready to accept connections"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start redis container: %v", err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(context.Background()) })

	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatalf("redis host: %v", err)
	}
	port, err := ctr.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("redis port: %v", err)
	}
	return fmt.Sprintf("%s:%s", host, port.Port())
}

// countingRedis wraps a real kv.Redis so a test can count the round trips one
// reservation costs without changing what the server executes.
type countingRedis struct {
	*kv.Redis
	mu    sync.Mutex
	names []string
	keys  [][]string
}

func newCountingRedis(t *testing.T, addr string) *countingRedis {
	t.Helper()
	s, err := kv.NewRedis(context.Background(), kv.RedisConfig{Addr: addr})
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &countingRedis{Redis: s}
}

func (c *countingRedis) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	c.mu.Lock()
	c.names = append(c.names, name)
	c.keys = append(c.keys, append([]string(nil), keys...))
	c.mu.Unlock()
	return c.Redis.RunScript(ctx, name, script, keys, args...)
}

func (c *countingRedis) calls() ([]string, [][]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.names...), append([][]string(nil), c.keys...)
}

func redisFixture(t *testing.T, addr string, rules ...appratelimit.Rule) (*Service, *countingRedis, *Policy) {
	t.Helper()
	pol := fix("prod-policy")
	pol.Meta.ID = "pol-1"
	var rl *appratelimit.RateLimit
	if len(rules) > 0 {
		rl = testRateLimit("rl-1", rules...)
		pol.Spec.RateLimitID = rl.Meta.ID
	}
	store := newCountingRedis(t, addr)
	return NewService(reserveSnap{pol: pol, rl: rl}, nil,
		pkgratelimit.New(store, discardLogger(), nil)), store, pol
}

// TestReserve_IsOneScriptOnRedis is the hot-path budget against the store
// production runs on: a token request with rate rules and a revocation check
// costs exactly one EVAL, every key it touches shares the team hash tag (so
// Redis Cluster can execute it at all), and the revocation key comes first so
// a revoked token answers 401 rather than the 429 an over-limit rule ahead of
// it would produce.
func TestReserve_IsOneScriptOnRedis(t *testing.T) {
	addr := startRedis(t)
	rules := []appratelimit.Rule{
		{Meter: appratelimit.MeterRequests, Amount: 1 << 20, Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow},
		{Meter: appratelimit.MeterTokens, Amount: 1 << 20, Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategySlidingWindow},
		{Meter: appratelimit.MeterTokensInput, Amount: 1 << 20, Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyTokenBucket},
	}
	svc, store, pol := redisFixture(t, addr, rules...)
	ctx := context.Background()

	res, err := svc.ReserveInbound(ctx, InboundInput{
		Policy: pol, TeamID: "team-1", TokenJTI: "jti-1",
		ProviderSlug: "prov", ModelSlug: "model", HostSlug: "host",
	})
	if err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if res == nil {
		t.Fatal("no reservation for a metered token request")
	}
	names, keys := store.calls()
	if len(names) != 1 || names[0] != "limit.reserve" {
		t.Fatalf("script calls = %v, want exactly one limit.reserve", names)
	}
	touched := keys[0]
	if want := RevokedKey("team-1", "jti-1"); len(touched) == 0 || touched[0] != want {
		t.Fatalf("first key = %v, want the revocation key %q first", touched, want)
	}
	for _, k := range touched {
		if !strings.HasPrefix(k, "limit:{team:team-1}:") {
			t.Errorf("key %q is outside the team hash tag; a real cluster refuses the script", k)
		}
	}
}

// A revoked token is refused inside that same single call — the denylist
// entry is read by the reservation, not by a second round trip before it.
func TestReserve_RevokedJTIOnRedis(t *testing.T) {
	addr := startRedis(t)
	svc, store, pol := redisFixture(t, addr,
		appratelimit.Rule{Meter: appratelimit.MeterRequests, Amount: 1 << 20,
			Window: appratelimit.Window(time.Hour), Strategy: appratelimit.StrategyFixedWindow})
	ctx := context.Background()

	if err := store.Set(ctx, RevokedKey("team-1", "jti-1"), []byte("1"), time.Hour); err != nil {
		t.Fatalf("write denylist entry: %v", err)
	}
	before, _ := store.calls()
	_, err := svc.ReserveInbound(ctx, InboundInput{Policy: pol, TeamID: "team-1", TokenJTI: "jti-1"})
	if !errors.Is(err, pkgratelimit.ErrRevoked) {
		t.Fatalf("err = %v, want ErrRevoked", err)
	}
	after, _ := store.calls()
	if got := len(after) - len(before); got != 1 {
		t.Fatalf("script calls = %d, want the revocation to ride the single reservation", got)
	}
	// A different token in the same team is unaffected.
	if _, err := svc.ReserveInbound(ctx, InboundInput{Policy: pol, TeamID: "team-1", TokenJTI: "jti-2"}); err != nil {
		t.Fatalf("second token: %v", err)
	}
}

// Reserve and Commit together cost one call when revocation is the only rule:
// there is no metered state for the commit-side script to return.
func TestReserveThenCommit_IsOneScriptOnRedis(t *testing.T) {
	addr := startRedis(t)
	svc, store, pol := redisFixture(t, addr)
	ctx := context.Background()

	res, err := svc.ReserveInbound(ctx, InboundInput{Policy: pol, TeamID: "team-1", TokenJTI: "jti-1"})
	if err != nil {
		t.Fatalf("ReserveInbound: %v", err)
	}
	if err := svc.CommitInbound(ctx, res, pkgratelimit.Observations{}); err != nil {
		t.Fatalf("CommitInbound: %v", err)
	}
	names, _ := store.calls()
	if len(names) != 1 {
		t.Fatalf("script calls = %v, want exactly 1 (Reserve only, Commit skipped)", names)
	}
}
