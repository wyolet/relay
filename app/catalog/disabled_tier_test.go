package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
)

// tierFixture is one host, its tier policy, and a key mirroring that tier.
type tierFixture struct {
	host *host.Host
	tier *policy.Policy
	key  *hostkey.HostKey
}

func newTierFixture(tierEnabled bool) tierFixture {
	hostID := meta.NewID()
	f := tierFixture{}
	f.host = &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: "upstream", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://upstream.example"},
	}
	f.tier = &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: policy.Spec{Enabled: &tierEnabled},
	}
	f.key = &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream-key", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: hostkey.Spec{
			HostID: hostID, PolicyID: f.tier.Meta.ID, Value: "sk",
			ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored},
		},
	}
	return f
}

func (f tierFixture) catalog(t *testing.T) *Catalog {
	t.Helper()
	c := New(provList{}, hostList{f.host}, polList{f.tier}, modList{},
		keyList{f.key}, rlList{}, rkList{}, rcList{}, bndList{})
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	return c
}

// D79: a host key whose tier policy is switched off KEEPS its place in the
// snapshot — evicting it would strand the key until the next full reload,
// since re-enabling the policy has nothing to reinsert. What denies it is the
// tier gate: PolicyAllowsCombo grants nothing for a policy that is not
// enabled, so the key is never selected while the tier is off and comes back
// the moment it returns.
func TestHostKeyOnDisabledTierStaysButIsDenied(t *testing.T) {
	assertDenied := func(t *testing.T, s *Snapshot, f tierFixture) {
		t.Helper()
		if _, ok := s.HostKey(f.key.Meta.ID); !ok {
			t.Error("host key was evicted; re-enabling the tier could not restore it")
		}
		if got := s.HostKeysForHost(f.host.Meta.ID); len(got) != 1 {
			t.Errorf("host pool = %v, want the key still present", names(got))
		}
		if s.PolicyAllowsCombo(f.tier.Meta.ID, meta.NewID(), f.host.Meta.ID) {
			t.Error("the tier gate admits a key whose policy is disabled")
		}
	}

	t.Run("build", func(t *testing.T) {
		f := newTierFixture(false)
		assertDenied(t, f.catalog(t).Current(), f)
	})

	t.Run("reconcile", func(t *testing.T) {
		f := newTierFixture(true)
		c := f.catalog(t)
		if !c.Current().PolicyAllowsCombo(f.tier.Meta.ID, meta.NewID(), f.host.Meta.ID) {
			t.Fatal("the enabled implicit-wildcard tier denies its own key")
		}
		off := false
		turnedOff := *f.tier
		turnedOff.Spec.Enabled = &off
		if err := c.ApplyPolicyUpsert(&turnedOff); err != nil {
			t.Fatalf("disable tier policy: %v", err)
		}
		assertDenied(t, c.Current(), f)
	})
}

// D79: toggling a tier policy off and on again must leave its host keys
// serving, with no full reload in between. Evicting the keys on disable
// stranded them: re-enable finds the policy present, so the absent-id
// recovery never fires and there is nothing to reinsert.
func TestTogglingATierPolicyKeepsItsHostKeysServing(t *testing.T) {
	f := newTierFixture(true)
	c := f.catalog(t)
	before := c.Current().Generation()

	off, on := false, true
	turnedOff := *f.tier
	turnedOff.Spec.Enabled = &off
	if err := c.ApplyPolicyUpsert(&turnedOff); err != nil {
		t.Fatalf("disable tier policy: %v", err)
	}
	if c.Current().PolicyAllowsCombo(f.tier.Meta.ID, meta.NewID(), f.host.Meta.ID) {
		t.Error("the tier gate still admits the key while its policy is off")
	}

	turnedOn := *f.tier
	turnedOn.Spec.Enabled = &on
	if err := c.ApplyPolicyUpsert(&turnedOn); err != nil {
		t.Fatalf("re-enable tier policy: %v", err)
	}
	s := c.Current()
	if _, ok := s.HostKey(f.key.Meta.ID); !ok {
		t.Fatal("the host key did not survive the toggle; it is stranded until a reload")
	}
	if got := s.HostKeysForHost(f.host.Meta.ID); len(got) != 1 {
		t.Fatalf("host pool = %v, want the key serving again", names(got))
	}
	if !s.PolicyAllowsCombo(f.tier.Meta.ID, meta.NewID(), f.host.Meta.ID) {
		t.Error("the tier gate still denies the key after its policy came back")
	}
	// Two incremental publishes, one per write — neither went through the
	// absent-id recovery, which rebuilds from every store.
	if got := s.Generation(); got != before+2 {
		t.Errorf("generation went %d → %d, want exactly two incremental publishes", before, got)
	}
}

// PolicyAllowsCombo is the tier gate and the explicit-grant test at once, so
// its answer for an id naming no enabled policy is what decides whether a
// stranded key serves traffic unmetered.
func TestPolicyAllowsCombo_RequiresAnEnabledPolicy(t *testing.T) {
	f := newTierFixture(true)
	m := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "m", Owner: meta.Owner{Kind: meta.OwnerSystem}},
	}
	c := f.catalog(t)
	s := c.Current()

	if s.PolicyAllowsCombo(meta.NewID(), m.Meta.ID, f.host.Meta.ID) {
		t.Error("an id naming no policy grants something")
	}
	if !s.PolicyAllowsCombo(f.tier.Meta.ID, m.Meta.ID, f.host.Meta.ID) {
		t.Error("an enabled implicit-wildcard policy grants nothing")
	}

	off := false
	turnedOff := *f.tier
	turnedOff.Spec.Enabled = &off
	if err := c.ApplyPolicyUpsert(&turnedOff); err != nil {
		t.Fatalf("disable tier policy: %v", err)
	}
	if c.Current().PolicyAllowsCombo(f.tier.Meta.ID, m.Meta.ID, f.host.Meta.ID) {
		t.Error("a disabled implicit-wildcard policy still grants everything")
	}
}
