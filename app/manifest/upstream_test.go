package manifest_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/manifest"
)

const upstreamYAML = `
apiVersion: relay.wyolet.dev/v1alpha3
kind: Account
metadata: {name: acme-anthropic, owner: {kind: system}}
spec: {host: anthropic-direct, orgId: org-1}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Pool
metadata: {name: bedrock-claude, owner: {kind: system}}
spec: {host: bedrock-us-east, credentials: [bedrock-key-prod]}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Budget
metadata: {name: monthly, owner: {kind: system}}
spec: {amountUsd: 2000}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: HostKey
metadata: {name: openai-key-1, owner: {kind: system}}
spec: {hostId: anthropic-direct, account: acme-anthropic, valueFrom: {kind: env, env: KEY}}
`

func TestUpstreamKinds_RoundTrip(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(upstreamYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(docs) != 4 || docs[0].Account == nil || docs[1].Pool == nil || docs[2].Budget == nil || docs[3].HostKey == nil {
		t.Fatalf("want Account, Pool, Budget, HostKey; got %v", docs)
	}

	a, err := manifest.ToAccount(*docs[0].Account, testResolver)
	if err != nil {
		t.Fatalf("ToAccount: %v", err)
	}
	if a.Spec.HostID != "host-bbb" || a.Spec.OrgID != "org-1" {
		t.Errorf("account spec = %+v", a.Spec)
	}
	if back := manifest.FromAccount(a, testRev); back.Spec.Host != "anthropic-direct" || back.Kind != "Account" {
		t.Errorf("FromAccount = %+v", back)
	}

	p, err := manifest.ToPool(*docs[1].Pool, testResolver)
	if err != nil {
		t.Fatalf("ToPool: %v", err)
	}
	if p.Spec.HostID != "host-ccc" || !reflect.DeepEqual(p.Spec.CredentialIDs, []string{"hk-hhh"}) || p.Spec.Selection != keypool.KeySelectionPrioritized {
		t.Errorf("pool spec = %+v (selection defaults to prioritized)", p.Spec)
	}
	if back := manifest.FromPool(p, testRev); back.Spec.Host != "bedrock-us-east" || back.Spec.Credentials[0] != "bedrock-key-prod" {
		t.Errorf("FromPool = %+v", back)
	}

	b, err := manifest.ToBudget(*docs[2].Budget, testResolver)
	if err != nil {
		t.Fatalf("ToBudget: %v", err)
	}
	if b.Spec.AmountUSD != "2000" || b.Spec.Period != budget.PeriodMonth || b.Spec.Mode != budget.ModeHard {
		t.Errorf("budget spec = %+v (period and mode default)", b.Spec)
	}

	k, err := manifest.ToHostKey(*docs[3].HostKey, testResolver)
	if err != nil {
		t.Fatalf("ToHostKey: %v", err)
	}
	if k.Spec.AccountID != "acc-mmm" {
		t.Errorf("host key account = %q", k.Spec.AccountID)
	}
	if back := manifest.FromHostKey(k, testRev); back.Spec.Account != "acme-anthropic" {
		t.Errorf("FromHostKey account = %q", back.Spec.Account)
	}
}

// A provider account written without an owner is the system's, like a host key written without one, so the pair needs no owner to match.
func TestProviderAccountWithoutOwnerIsTheSystems(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader("apiVersion: relay.wyolet.dev/v1alpha3\nkind: Account\nmetadata: {name: acme}\nspec: {host: anthropic-direct}\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a, err := manifest.ToAccount(*docs[0].Account, testResolver)
	if err != nil {
		t.Fatalf("ToAccount: %v", err)
	}
	if a.Meta.Owner.Kind != "system" {
		t.Errorf("owner = %+v, want the system", a.Meta.Owner)
	}
}

func TestUpstreamKinds_UnknownNames(t *testing.T) {
	docs, err := manifest.Parse(strings.NewReader(upstreamYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var notFound *manifest.RefNotFoundError
	if _, err := manifest.ToAccount(*docs[0].Account, manifest.MapResolver{}); !errors.As(err, &notFound) {
		t.Errorf("account with unknown host: %v", err)
	}
	if _, err := manifest.ToPool(*docs[1].Pool, manifest.MapResolver{Hosts: testResolver.Hosts}); !errors.As(err, &notFound) {
		t.Errorf("pool with unknown credential: %v", err)
	}
	if _, err := manifest.ToHostKey(*docs[3].HostKey, manifest.MapResolver{Hosts: testResolver.Hosts}); !errors.As(err, &notFound) {
		t.Errorf("host key with unknown account: %v", err)
	}
}

// A policy binding names its accounts, which resolve by name and render back.
func TestPolicyBinding_AccountsRoundTrip(t *testing.T) {
	const doc = `
apiVersion: relay.wyolet.dev/v1alpha3
kind: PolicyBinding
metadata: {name: tiers, owner: {kind: system}}
spec:
  policy: cheap-tier
  accounts: [acme-anthropic]
`
	docs, err := manifest.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := manifest.ToPolicyBinding(*docs[0].PolicyBinding, testResolver)
	if err != nil {
		t.Fatalf("ToPolicyBinding: %v", err)
	}
	if !reflect.DeepEqual(b.Spec.AccountIDs, []string{"acc-mmm"}) {
		t.Fatalf("accounts = %v", b.Spec.AccountIDs)
	}
	if back := manifest.FromPolicyBinding(b, testRev); !reflect.DeepEqual(back.Spec.Accounts, []string{"acme-anthropic"}) {
		t.Errorf("rendered accounts = %v", back.Spec.Accounts)
	}
	var notFound *manifest.RefNotFoundError
	if _, err := manifest.ToPolicyBinding(*docs[0].PolicyBinding, manifest.MapResolver{Policies: testResolver.Policies}); !errors.As(err, &notFound) {
		t.Errorf("binding an unknown account: %v", err)
	}
}

// The v0.20 fields a policy and a binding no longer have are refused, not dropped: a silently dropped restriction would widen access.
func TestRemovedFieldsAreRefused(t *testing.T) {
	for field, doc := range map[string]string{
		"credentials":           "kind: Policy\nmetadata: {name: p}\nspec: {credentials: [k]}",
		"includeDeprecated":     "kind: Policy\nmetadata: {name: p}\nspec: {includeDeprecated: true}",
		"payloadLoggingEnabled": "kind: Policy\nmetadata: {name: p}\nspec: {payloadLoggingEnabled: true}",
		"subjects":              "kind: PolicyBinding\nmetadata: {name: b}\nspec: {policy: p, subjects: [{kind: team, name: t}]}",
		"project":               "kind: PolicyBinding\nmetadata: {name: b}\nspec: {policy: p, project: x, accounts: [a]}",
	} {
		_, err := manifest.Parse(strings.NewReader("apiVersion: relay.wyolet.dev/v1alpha3\n" + doc))
		if err == nil || !strings.Contains(err.Error(), "spec."+field) {
			t.Errorf("%s: err = %v, want a refusal naming spec.%s", field, err, field)
		}
	}
}
