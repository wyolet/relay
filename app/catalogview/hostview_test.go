package catalogview

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/meta"
)

func TestHostKeyList(t *testing.T) {
	off := false
	fromEnv := keyOf("k-alice", "host-a", alice)
	fromEnv.Spec.ValueFrom = hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "OPENAI_KEY"}
	fromEnv.Resolved = "sk-ant-api03-ABCDEFGHIJKLMNOP1234"
	stored := keyOf("k-bob", "host-a", bob)
	stored.Spec.ValueFrom, stored.Spec.Enabled, stored.Spec.DefaultTier = hostkey.ValueFrom{Kind: hostkey.ValueKindStored}, &off, "tier-2"
	stored.Resolved = "an-unknown-format-secret-9876"
	short := keyOf("k-operator", "host-a", operator)
	short.Resolved = "sk-shorter-than-24-char"
	svc := newService(hostOf("host-a"), hostOf("host-b"), short, stored, fromEnv, keyOf("k-elsewhere", "host-b", alice))

	// The secret's start is for whoever may edit the key; anyone else sees the format prefix and the last four.
	aliceKey := HostKeyView{ID: "key:k-alice", Name: "k-alice", Kind: "env", Enabled: true}
	bobKey := HostKeyView{ID: "key:k-bob", Name: "k-bob", Kind: "stored", DefaultTier: "tier-2"}
	withPrefix := func(v HostKeyView, prefix string) HostKeyView {
		v.Prefix = prefix
		return v
	}
	ownedByAlice := func(_, _ string, owner meta.Owner) bool { return owner == alice }
	for _, c := range []struct {
		name          string
		visible, owns func(kind, id string, owner meta.Owner) bool
		want          []HostKeyView
	}{
		{"single user owns every key", nil, nil, []HostKeyView{withPrefix(aliceKey, "sk-ant-a"), withPrefix(bobKey, "an-unkno"), keyView("k-operator")}},
		{"others' keys show a hint", nil, ownedByAlice, []HostKeyView{withPrefix(aliceKey, "sk-ant-a"), withPrefix(bobKey, "...9876"), keyView("k-operator")}},
		{"own key seen without edit rights", nil, func(string, string, meta.Owner) bool { return false }, []HostKeyView{withPrefix(aliceKey, "sk-ant-...1234"), withPrefix(bobKey, "...9876"), keyView("k-operator")}},
		{"hidden rows drop out", asAlice, ownedByAlice, []HostKeyView{withPrefix(aliceKey, "sk-ant-a")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc.Visible, svc.Owns = c.visible, c.owns
			viewed(t, "host-a", hostRef("host-a"), c.want)(svc.HostKeyList(context.Background(), "host-a"))
		})
	}
}

func TestHostPolicies_OwnTierPresetsOnly(t *testing.T) {
	off := false
	disabledRL := rlOf("off", 5)
	disabledRL.Spec.Enabled = &off
	disabledTier := policyOf("a-disabled", hostTier("host-a"), nil)
	disabledTier.Spec.Enabled = &off
	tier := policyOf("tier", hostTier("host-a"), nil)
	tier.Meta.DisplayName = "Tier 1"
	svc := newService(
		hostOf("host-a"), hostOf("host-b"), rlOf("rpm", 50), disabledRL,
		limitedBy(limitedBy(limitedBy(limitedBy(tier, "rpm"), "rpm", "openai/gpt-4o"), "off"), "deleted"),
		disabledTier,
		policyOf("other-host", hostTier("host-b"), nil),
		policyOf("cust", operator, nil),
	)
	viewed(t, "host-a", hostRef("host-a"), []HostPolicyRow{
		{ID: "policy:a-disabled", Name: "a-disabled", RateLimits: []PolicyRateLimitRow{}},
		{ID: "policy:tier", Name: "tier", DisplayName: "Tier 1", Enabled: true, RateLimits: []PolicyRateLimitRow{
			{ID: "rl:rpm", Name: "rpm", Default: true, Limits: []Limit{limit(50)}},
			{ID: "rl:rpm", Name: "rpm", Models: []string{"openai/gpt-4o"}, Limits: []Limit{limit(50)}},
			// A disabled rate limit adds no rule; a deleted one keeps its link's id.
			{ID: "rl:off", Name: "off", Default: true, Limits: []Limit{}},
			{ID: "rl:deleted", Default: true, Limits: []Limit{}},
		}},
	})(svc.HostPolicies(context.Background(), "host:host-a"))
}
