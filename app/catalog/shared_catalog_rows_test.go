package catalog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/ratelimit"
)

type hostsByID map[string]*host.Host

func (m hostsByID) Get(_ context.Context, id string) (*host.Host, error) { return m[id], nil }

// captureErrorLog routes slog's default logger to a buffer for the test.
func captureErrorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func ownedBy(o meta.Owner, m meta.Metadata) meta.Metadata {
	m.Owner = o
	return m
}

// A host or binding stored with a tenant owner (written before the shared
// catalog rule) stays out of the snapshot on a full reload, and says so.
func TestTenantOwnedHostAndBindingExcludedOnReload(t *testing.T) {
	log := captureErrorLog(t)
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()

	userHost := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "my-box", Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}},
		Spec: host.Spec{BaseURL: "http://10.0.0.5:11434"},
	}
	teamHost := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "team-box", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}},
		Spec: host.Spec{BaseURL: "http://10.0.0.6:11434"},
	}
	hosts = append(hosts, userHost, teamHost)
	userBinding := *bnds[1]
	userBinding.Meta = ownedBy(meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}, userBinding.Meta)
	bnds[1] = &userBinding

	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	s := c.Current()
	for _, h := range []*host.Host{userHost, teamHost} {
		if _, ok := s.Host(h.Meta.ID); ok {
			t.Errorf("%s-owned host %q is in the snapshot", h.Meta.Owner.Kind, h.Meta.Name)
		}
		if _, ok := s.HostByName(h.Meta.Name); ok {
			t.Errorf("%s-owned host %q resolves by name", h.Meta.Owner.Kind, h.Meta.Name)
		}
	}
	if _, ok := s.Binding(userBinding.Meta.ID); ok {
		t.Error("user-owned binding is in the snapshot")
	}
	if got := s.BindingsForModel(userBinding.Spec.ModelID); len(got) != 0 {
		t.Errorf("model of the user-owned binding still has %d bindings", len(got))
	}
	if _, ok := s.Binding(bnds[0].Meta.ID); !ok {
		t.Error("system-owned binding missing")
	}
	if _, ok := s.Host(hosts[0].Meta.ID); !ok {
		t.Error("system-owned host missing")
	}
	for _, want := range []string{"name=my-box", "name=team-box", "name=" + userBinding.Meta.Name, "excluded from routing"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("error log does not mention %q:\n%s", want, log)
		}
	}
}

// The incremental paths a NOTIFY drives keep the same rule: a host or
// binding that turns up tenant-owned leaves the snapshot.
func TestTenantOwnedHostAndBindingExcludedOnIncrementalUpsert(t *testing.T) {
	t.Run("host", func(t *testing.T) {
		log := captureErrorLog(t)
		provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
		c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
		if err := c.Reload(context.Background()); err != nil {
			t.Fatalf("reload: %v", err)
		}
		stored := *hosts[0]
		stored.Meta = ownedBy(meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}, stored.Meta)
		hosts[0] = &stored

		l := &Listener{cat: c, deb: newDebouncer(0), stores: listenerStores{host: hostsByID{stored.Meta.ID: &stored}}}
		if err := l.applyEvent(context.Background(), drainedEvent{Kind: "host", Op: "update", ID: stored.Meta.ID}); err != nil {
			t.Fatalf("applyEvent: %v", err)
		}
		if _, ok := c.Current().Host(stored.Meta.ID); ok {
			t.Fatal("user-owned host is in the snapshot after an incremental upsert")
		}
		if !strings.Contains(log.String(), "name="+stored.Meta.Name) {
			t.Errorf("error log does not name the host:\n%s", log)
		}

		// A new tenant-owned host never enters either.
		fresh := &host.Host{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "my-box", Owner: meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}},
			Spec: host.Spec{BaseURL: "http://10.0.0.5:11434"},
		}
		if err := c.ApplyHostUpsert(fresh); err != nil {
			t.Fatalf("ApplyHostUpsert: %v", err)
		}
		if _, ok := c.Current().Host(fresh.Meta.ID); ok {
			t.Fatal("new project-owned host is in the snapshot")
		}
	})

	t.Run("binding", func(t *testing.T) {
		log := captureErrorLog(t)
		provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
		c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
		if err := c.Reload(context.Background()); err != nil {
			t.Fatalf("reload: %v", err)
		}
		stored := *bnds[0]
		stored.Meta = ownedBy(meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}, stored.Meta)
		bnds[0] = &stored

		l := &Listener{cat: c, deb: newDebouncer(0)}
		if err := l.applyEvent(context.Background(), drainedEvent{Kind: "hostbinding", Op: "update", ID: stored.Meta.ID}); err != nil {
			t.Fatalf("applyEvent: %v", err)
		}
		s := c.Current()
		if _, ok := s.Binding(stored.Meta.ID); ok {
			t.Fatal("user-owned binding is in the snapshot after its NOTIFY")
		}
		if _, ok := s.Binding(bnds[1].Meta.ID); !ok {
			t.Error("system-owned binding missing")
		}
		if !strings.Contains(log.String(), "name="+stored.Meta.Name) {
			t.Errorf("error log does not name the binding:\n%s", log)
		}
	})
}

// The proxy and receiver buckets are looked up by name; a row of that name
// owned by anyone but the deployment must not stand in for them.
func TestSystemRateLimitByNameIgnoresNonSystemRows(t *testing.T) {
	rule := []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 1, Window: 60, Strategy: ratelimit.StrategyTokenBucket}}
	for _, owner := range []meta.Owner{
		{Kind: meta.OwnerUser, ID: "u-1"},
		{Kind: meta.OwnerUser},
		{Kind: meta.OwnerHost, ID: meta.NewID()},
	} {
		rl := &ratelimit.RateLimit{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "inference-api-proxy", Owner: owner},
			Spec: ratelimit.Spec{Rules: rule},
		}
		s := Build(nil, nil, nil, nil, nil, nil, []*ratelimit.RateLimit{rl}, nil, nil)
		if _, ok := s.RateLimit(rl.Meta.ID); !ok {
			t.Fatalf("%s-owned rate limit not loaded; the lookup is not what is under test", owner.Kind)
		}
		if got, ok := s.SystemRateLimitByName("inference-api-proxy"); ok {
			t.Errorf("%s-owned row returned as the system limit: %+v", owner.Kind, got.Meta)
		}
	}

	sys := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "inference-api-proxy", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: ratelimit.Spec{Rules: rule},
	}
	s := Build(nil, nil, nil, nil, nil, nil, []*ratelimit.RateLimit{sys}, nil, nil)
	if got, ok := s.SystemRateLimitByName("inference-api-proxy"); !ok || got.Meta.ID != sys.Meta.ID {
		t.Fatalf("system row not returned: %v %v", got, ok)
	}
}
