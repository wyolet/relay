package catalog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
)

// addKeys sanitises against snapshot membership: a policy dropped
// because its project is missing must take the key naming it with it, the way
// the incremental cascade does.
func TestAddKeysSanitizesAgainstSnapshotMembership(t *testing.T) {
	orphanProject := meta.NewID()
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "orphan", Owner: meta.Owner{Kind: meta.OwnerProject, ID: orphanProject}},
	}
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "k", Owner: meta.Owner{Kind: meta.OwnerUser}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: meta.NewID()}, PolicyID: pol.Meta.ID, KeyHash: "h"},
	}
	c := New(provList{}, hostList{}, polList{pol}, modList{}, keyList{}, rlList{},
		rkList{k}, rcList{}, bndList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := c.Current().KeyByHash("h"); got != nil {
		t.Fatal("the key survived a policy the build dropped")
	}
}

// A rate limit named only by an RLBinding is a registered parent, so
// deleting it re-sanitizes the policies pointing at it.
func TestRLBindingRateLimitIsARegisteredRef(t *testing.T) {
	rl := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rl", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 10,
			Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyFixedWindow,
		}}},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "p", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{RLBindings: []policy.RLBinding{{Models: []string{"acme/m"}, RateLimitID: rl.Meta.ID}}},
	}
	c := New(provList{}, hostList{}, polList{pol}, modList{}, keyList{}, rlList{rl},
		rkList{}, rcList{}, bndList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	deps := c.Current().Dependents(refRateLimit, rl.Meta.ID)
	if len(deps) != 1 || deps[0].ID != pol.Meta.ID {
		t.Fatalf("dependents of the rate limit = %v, want the policy naming it in an RLBinding", deps)
	}
	if err := c.ApplyRateLimitDelete(rl.Meta.ID); err != nil {
		t.Fatalf("delete rate limit: %v", err)
	}
	got, ok := c.Current().Policy(pol.Meta.ID)
	if !ok {
		t.Fatal("the policy was dropped, not re-sanitized")
	}
	if len(got.Spec.RLBindings) != 0 {
		t.Fatalf("rlBindings = %v, want the dangling ref dropped", got.Spec.RLBindings)
	}
}

// Re-pointing a tier policy at another host invalidates the host keys
// that mirror it; the upsert must re-check its dependents, not just land.
func TestPolicyUpsertRechecksDependents(t *testing.T) {
	hostA, hostB := meta.NewID(), meta.NewID()
	hA := &host.Host{Meta: meta.Metadata{ID: hostA, Name: "a", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://a"}}
	hB := &host.Host{Meta: meta.Metadata{ID: hostB, Name: "b", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "http://b"}}
	tier := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostA}}}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "k", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{HostID: hostA, PolicyID: tier.Meta.ID, Value: "sk", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	c := New(provList{}, hostList{hA, hB}, polList{tier}, modList{}, keyList{hk}, rlList{},
		rkList{}, rcList{}, bndList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if _, ok := c.Current().HostKey(hk.Meta.ID); !ok {
		t.Fatal("host key missing before the change")
	}
	moved := *tier
	moved.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: hostB}
	if err := c.ApplyPolicyUpsert(&moved); err != nil {
		t.Fatalf("re-point tier policy: %v", err)
	}
	if _, ok := c.Current().HostKey(hk.Meta.ID); ok {
		t.Fatal("the host key survived a tier policy that no longer belongs to its host")
	}
}

// A grant the parser cannot read silently stops granting; only a log
// tells the operator why the policy suddenly allows less.
func TestUnparseableModelRefIsLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "p", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policy.Spec{Models: []string{"acme/model@"}},
	}
	c := New(provList{}, hostList{}, polList{pol}, modList{}, keyList{}, rlList{},
		rkList{}, rcList{}, bndList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !strings.Contains(buf.String(), "policy model ref unparseable") {
		t.Fatalf("no warning logged for an unparseable grant; log was:\n%s", buf.String())
	}
}
