package catalogview

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

func TestPolicyModels(t *testing.T) {
	ctx := context.Background()
	openai := ProviderRef{ID: "prov:openai", Name: "openai"}
	viewed(t, "grant and limits", policyRef("tier-1", OwnerRef{Kind: "user"}), []PolicyBindingRow{
		{Provider: openai, Host: tier1Host(), Model: modelRef("gpt-4o"), Binding: bindView("gpt-4o", "openai"), MatchedBy: []string{"openai/gpt-4o"}, Limits: []Limit{limit(100)}},
	})(tier1().PolicyModels(ctx, "tier-1"))

	stray := modelOf("stray")
	stray.Meta.Owner.ID = "prov:deleted"
	entries := policyOf("entries", operator, []string{"openai", "not a/valid/ref/at@all@", "openai/o1", "openai/gpt-4o@azure"}, "azure-keys", "openai-keys")
	entries.Meta.DisplayName = "Entries"
	svc := newService(
		provOf("openai"), hostOf("openai"), hostOf("azure"), hostOf("unreached"), modelOf("o1"), modelOf("gpt-4o"), stray,
		bindOf("o1", "openai"), bindOf("gpt-4o", "openai"), bindOf("gpt-4o", "azure"), bindOf("gpt-4o", "unreached"), bindOf("gpt-4o", "deleted"), bindOf("stray", "azure"),
		poolOf("azure-keys", "azure", operator), poolOf("openai-keys", "openai", operator),
		entries, policyOf("everything", operator, nil), policyOf("azure-tier", hostTier("azure"), nil),
	)
	row := func(prov ProviderRef, modelName, hostName string, matchedBy ...string) PolicyBindingRow {
		return PolicyBindingRow{Provider: prov, Host: hostRef(hostName), Model: modelRef(modelName), Binding: bindView(modelName, hostName), MatchedBy: matchedBy, Limits: []Limit{}}
	}
	// Rows name the stored entries that allow them, on the hosts the pools reach, by model then host.
	entriesRef := policyRef("entries", OwnerRef{Kind: "user"})
	entriesRef.DisplayName = "Entries"
	viewed(t, "matching entries", entriesRef, []PolicyBindingRow{
		row(openai, "gpt-4o", "azure", "openai", "openai/gpt-4o@azure"),
		row(openai, "gpt-4o", "openai", "openai"),
		row(openai, "o1", "openai", "openai", "openai/o1"),
	})(svc.PolicyModels(ctx, "entries"))
	viewed(t, "no model list, no pools", policyRef("everything", OwnerRef{Kind: "user"}), []PolicyBindingRow{
		row(openai, "gpt-4o", "azure", "*"),
		row(openai, "gpt-4o", "openai", "*"),
		row(openai, "gpt-4o", "unreached", "*"),
		row(openai, "o1", "openai", "*"),
		row(ProviderRef{}, "stray", "azure", "*"),
	})(svc.PolicyModels(ctx, "everything"))
	azureTier := policyRef("azure-tier", OwnerRef{Kind: "host", ID: "host:azure", Name: "azure"})
	viewed(t, "tier preset: its own host", azureTier, []PolicyBindingRow{
		row(openai, "gpt-4o", "azure", "*"),
		row(ProviderRef{}, "stray", "azure", "*"),
	})(svc.PolicyModels(ctx, "policy:azure-tier"))

	viewed(t, "tier preset exclusions", azureTier, []PolicyModelExclusion{
		{Model: modelRef("o1"), Reason: "host-tier policy's host has no binding for this model"},
	})(svc.PolicyModelExclusions(ctx, "azure-tier"))
	viewed(t, "entries exclusions", entriesRef, []PolicyModelExclusion{
		{Model: modelRef("stray"), Reason: "policy's Models grant matches this model on no reachable host"},
	})(svc.PolicyModelExclusions(ctx, "entries"))
	svc.Policies = fPolicies{policyOf("unreached-only", operator, nil, "deleted-pool")}
	viewed(t, "pools reach nothing", policyRef("unreached-only", OwnerRef{Kind: "user"}), []PolicyModelExclusion{
		{Model: modelRef("gpt-4o"), Reason: "policy's pools reach no host that binds this model"},
		{Model: modelRef("o1"), Reason: "policy's pools reach no host that binds this model"},
		{Model: modelRef("stray"), Reason: "policy's pools reach no host that binds this model"},
	})(svc.PolicyModelExclusions(ctx, "unreached-only"))
}

func TestPolicyHosts(t *testing.T) {
	ctx := context.Background()
	off := false
	disabledKey := keyOf("ka", "host-a", operator)
	disabledKey.Spec.Enabled = &off
	// solo is served on host-a only; dual on both.
	svc := newService(
		provOf("openai"), hostOf("host-a"), hostOf("host-b"), hostOf("host-c"), modelOf("solo"), modelOf("dual"),
		bindOf("solo", "host-a"), bindOf("dual", "host-a"), bindOf("dual", "host-b"),
		disabledKey, keyOf("kb", "host-b", operator), keyOf("kb2", "host-b", operator),
		poolOf("pool-a", "host-a", operator, "ka", "deleted-key"), poolOf("pool-b", "host-b", operator, "kb", "kb2", "kb"), poolOf("pool-c", "host-c", operator),
		policyOf("both", operator, nil, "pool-a", "pool-b", "pool-c"),
		policyOf("only-a", operator, nil, "pool-a"),
		policyOf("also-a", operator, nil, "pool-a", "deleted-pool"),
		policyOf("no-pools", operator, nil),
	)
	ka := HostKeyRef{ID: "key:ka", Name: "ka", SharedWithPolicyCount: 2}
	both := policyRef("both", OwnerRef{Kind: "user"})
	viewed(t, "keys per host", both, []PolicyHostRow{
		{Host: hostRef("host-a"), HostKeys: []HostKeyRef{ka}, Requirement: "required"},
		{Host: hostRef("host-b"), HostKeys: []HostKeyRef{keyRef("kb"), keyRef("kb2")}, Requirement: "optional"},
		// A pool with no key still reaches its host.
		{Host: hostRef("host-c"), HostKeys: []HostKeyRef{}, Requirement: "optional"},
	})(svc.PolicyHosts(ctx, "both"))
	viewed(t, "one pool", policyRef("only-a", OwnerRef{Kind: "user"}), []PolicyHostRow{
		{Host: hostRef("host-a"), HostKeys: []HostKeyRef{ka}, Requirement: "required"},
	})(svc.PolicyHosts(ctx, "only-a"))
	// Without pools the policy is listed on every host serving a model it allows.
	viewed(t, "no pools", policyRef("no-pools", OwnerRef{Kind: "user"}), []PolicyHostRow{
		{Host: hostRef("host-a"), HostKeys: []HostKeyRef{}, Requirement: "required"},
		{Host: hostRef("host-b"), HostKeys: []HostKeyRef{}, Requirement: "optional"},
	})(svc.PolicyHosts(ctx, "no-pools"))
}

// A pool naming an account reaches that account's keys; a share reaches the real account's, read-only even when the caller may not see them.
func TestPolicyHosts_PoolAccounts(t *testing.T) {
	ctx := context.Background()
	moved := accountOf("moved", "other-host", bob, "")
	svc := newService(
		hostOf("h"),
		inAccount(keyOf("own-key", "h", alice), "own"),
		inAccount(keyOf("bobs-key", "h", bob), "bobs"),
		inAccount(keyOf("moved-key", "h", bob), "moved"),
		accountOf("own", "h", alice, ""), accountOf("empty", "h", alice, ""), accountOf("bobs", "h", bob, ""),
		accountOf("bobs-for-alice", "h", alice, "bobs"),
		// A share whose real account is at another host serves nothing.
		moved, accountOf("moved-for-alice", "h", alice, "moved"),
		drawingOn(poolOf("p-own", "h", alice), "own"),
		drawingOn(poolOf("p-share", "h", alice), "bobs-for-alice"),
		drawingOn(poolOf("p-empty", "h", alice), "empty", "deleted"),
		drawingOn(poolOf("p-all", "h", alice, "bobs-key"), "bobs-for-alice", "moved-for-alice", "own"),
		policyOf("via-own", alice, nil, "p-own"), policyOf("via-share", alice, nil, "p-share"),
		policyOf("via-empty", alice, nil, "p-empty"), policyOf("via-all", alice, nil, "p-all"),
	)
	svc.Visible = asAlice
	aliceOwner := OwnerRef{Kind: "user", ID: "u-alice"}
	ownKey, bobsKey := HostKeyRef{ID: "key:own-key", Name: "own-key", Enabled: true, SharedWithPolicyCount: 1}, HostKeyRef{ID: "key:bobs-key", Name: "bobs-key", Enabled: true, SharedWithPolicyCount: 1, ReadOnly: true}
	hostRow := func(keys ...HostKeyRef) []PolicyHostRow {
		return []PolicyHostRow{{Host: hostRef("h"), HostKeys: append([]HostKeyRef{}, keys...), Requirement: "optional"}}
	}
	viewed(t, "own account", policyRef("via-own", aliceOwner), hostRow(ownKey))(svc.PolicyHosts(ctx, "via-own"))
	viewed(t, "shared account", policyRef("via-share", aliceOwner), hostRow(bobsKey))(svc.PolicyHosts(ctx, "via-share"))
	viewed(t, "account with no keys", policyRef("via-empty", aliceOwner), hostRow())(svc.PolicyHosts(ctx, "via-empty"))
	// A key reached through a share and listed directly counts once, at its first place.
	viewed(t, "accounts then keys", policyRef("via-all", aliceOwner), hostRow(bobsKey, ownKey))(svc.PolicyHosts(ctx, "via-all"))

	// With bob's personal accounts off, his keys serve no one: through the share or listed directly.
	svc.PersonalAccountOff = func(userID string) bool { return userID == bob.ID }
	ownKey.SharedWithPolicyCount = 1
	viewed(t, "bob off", policyRef("via-all", aliceOwner), hostRow(ownKey))(svc.PolicyHosts(ctx, "via-all"))
	viewed(t, "bob off, share alone", policyRef("via-share", aliceOwner), hostRow())(svc.PolicyHosts(ctx, "via-share"))
}

func TestPolicyProjections_InvisiblePolicyIsNotFound(t *testing.T) {
	ctx := context.Background()
	svc := tier1()
	svc.Policies = fPolicies{policyOf("bob-pol", bob, nil), policyOf("alice-pol", alice, nil)}
	svc.Visible = func(kind, id string, owner meta.Owner) bool {
		if kind != "policy" || id != "policy:"+map[meta.Owner]string{alice: "alice-pol", bob: "bob-pol"}[owner] {
			t.Errorf("Visible(%q, %q, %+v): want the policy's kind, id and owner", kind, id, owner)
		}
		return asAlice(kind, id, owner)
	}
	for name, err := range map[string]error{
		"PolicyModels":          third(svc.PolicyModels(ctx, "bob-pol")),
		"PolicyModelExclusions": third(svc.PolicyModelExclusions(ctx, "bob-pol")),
		"PolicyHosts":           third(svc.PolicyHosts(ctx, "policy:bob-pol")),
		"PolicyRateLimits":      third(svc.PolicyRateLimits(ctx, "bob-pol")),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s(bob-pol) err = %v, want ErrNotFound", name, err)
		}
	}
	viewed(t, "alice's own", policyRef("alice-pol", OwnerRef{Kind: "user", ID: "u-alice"}), []PolicyBindingRow{
		{Provider: ProviderRef{ID: "prov:openai", Name: "openai"}, Host: tier1Host(), Model: modelRef("gpt-4o"), Binding: bindView("gpt-4o", "openai"), MatchedBy: []string{"*"}, Limits: []Limit{}},
	})(svc.PolicyModels(ctx, "alice-pol"))
}
