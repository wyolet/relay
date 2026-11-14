package manifest_test

import (
	"errors"
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/meta"
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

// Names resolve to ids, a pool's selection and a budget's period and mode take their defaults, and the exports name everything again.
func TestUpstreamKinds_RoundTrip(t *testing.T) {
	docs := parseDocs(t, upstreamYAML, 4)
	system := meta.Owner{Kind: meta.OwnerSystem}

	a := translated(t, "ToAccount", &account.Account{
		Meta: meta.Metadata{Name: "acme-anthropic", Owner: system},
		Spec: account.Spec{HostID: "host-bbb", OrgID: "org-1"},
	})(manifest.ToAccount(*docs[0].Account, testResolver))
	exported(t, "FromAccount", *docs[0].Account, manifest.FromAccount(a, testRev))

	p := translated(t, "ToPool", &pool.Pool{
		Meta: meta.Metadata{Name: "bedrock-claude", Owner: system},
		Spec: pool.Spec{HostID: "host-ccc", CredentialIDs: []string{"hk-hhh"}, Selection: keypool.KeySelectionPrioritized},
	})(manifest.ToPool(*docs[1].Pool, testResolver))
	wantPool := *docs[1].Pool
	wantPool.Spec.Selection = "prioritized"
	exported(t, "FromPool", wantPool, manifest.FromPool(p, testRev))

	b := translated(t, "ToBudget", &budget.Budget{
		Meta: meta.Metadata{Name: "monthly", Owner: system},
		Spec: budget.Spec{AmountUSD: "2000", Period: budget.PeriodMonth, Mode: budget.ModeHard},
	})(manifest.ToBudget(*docs[2].Budget, testResolver))
	wantBudget := *docs[2].Budget
	wantBudget.Spec.Period, wantBudget.Spec.Mode = "month", "hard"
	exported(t, "FromBudget", wantBudget, manifest.FromBudget(b, testRev))

	k := translated(t, "ToHostKey", &hostkey.HostKey{
		Meta: meta.Metadata{Name: "openai-key-1", Owner: system},
		Spec: hostkey.Spec{HostID: "host-bbb", AccountID: "acc-mmm", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "KEY"}},
	})(manifest.ToHostKey(*docs[3].HostKey, testResolver))
	exported(t, "FromHostKey", *docs[3].HostKey, manifest.FromHostKey(k, testRev))

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"account at an unknown host", second(manifest.ToAccount(*docs[0].Account, manifest.MapResolver{})), `account "acme-anthropic": host "anthropic-direct" not found`},
		{"pool at an unknown host", second(manifest.ToPool(*docs[1].Pool, manifest.MapResolver{})), `pool "bedrock-claude": host "bedrock-us-east" not found`},
		{"pool of an unknown key", second(manifest.ToPool(*docs[1].Pool, manifest.MapResolver{Hosts: testResolver.Hosts})), `pool "bedrock-claude": credential "bedrock-key-prod" not found`},
		{"host key in an unknown account", second(manifest.ToHostKey(*docs[3].HostKey, manifest.MapResolver{Hosts: testResolver.Hosts})), `hostkey "openai-key-1": account "acme-anthropic" not found`},
	} {
		var notFound *manifest.RefNotFoundError
		if got := errText(tc.err); got != tc.want || !errors.As(tc.err, &notFound) {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}

func second[R any](_ R, err error) error { return err }

// A provider account written without an owner is the system's, like a host key written without one, so the pair needs no owner to match.
func TestProviderAccountWithoutOwnerIsTheSystems(t *testing.T) {
	doc := parseOne(t, "apiVersion: relay.wyolet.dev/v1alpha3\nkind: Account\nmetadata: {name: acme}\nspec: {host: anthropic-direct}\n")
	translated(t, "ToAccount", &account.Account{
		Meta: meta.Metadata{Name: "acme", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: account.Spec{HostID: "host-bbb"},
	})(manifest.ToAccount(*doc.Account, testResolver))
}

// A policy binding names its accounts, which resolve by name and render back.
func TestPolicyBinding_AccountsRoundTrip(t *testing.T) {
	doc := parseOne(t, `
apiVersion: relay.wyolet.dev/v1alpha3
kind: PolicyBinding
metadata: {name: tiers, owner: {kind: system}}
spec:
  policy: cheap-tier
  accounts: [acme-anthropic]
`)
	b := translated(t, "ToPolicyBinding", &policybinding.PolicyBinding{
		Meta: meta.Metadata{Name: "tiers", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: policybinding.Spec{PolicyID: "policy-ddd", AccountIDs: []string{"acc-mmm"}},
	})(manifest.ToPolicyBinding(*doc.PolicyBinding, testResolver))
	exported(t, "FromPolicyBinding", *doc.PolicyBinding, manifest.FromPolicyBinding(b, testRev))

	_, err := manifest.ToPolicyBinding(*doc.PolicyBinding, manifest.MapResolver{Policies: testResolver.Policies})
	var notFound *manifest.RefNotFoundError
	if got, want := errText(err), `policybinding "tiers": account "acme-anthropic" not found`; got != want || !errors.As(err, &notFound) {
		t.Errorf("binding an unknown account:\n got %q\nwant %s", got, want)
	}
}

// The v0.20 fields a policy and a binding no longer have are refused, not dropped: a silently dropped restriction would widen access.
func TestRemovedPolicyAndBindingFieldsAreRefused(t *testing.T) {
	for _, tc := range []struct{ doc, want string }{
		{"kind: Policy\nmetadata: {name: p}\nspec: {credentials: [k]}", "wire: doc 0 kind=Policy name=p: Policy no longer has spec.credentials (use pools (a Pool of the one host key))"},
		{"kind: Policy\nmetadata: {name: p}\nspec: {includeDeprecated: true}", "wire: doc 0 kind=Policy name=p: Policy no longer has spec.includeDeprecated (use nothing: models grant deprecated models like any other)"},
		{"kind: Policy\nmetadata: {name: p}\nspec: {payloadLoggingEnabled: true}", "wire: doc 0 kind=Policy name=p: Policy no longer has spec.payloadLoggingEnabled (use payloadLogging on the accounts the policy is bound to)"},
		{"kind: PolicyBinding\nmetadata: {name: b}\nspec: {policy: p, subjects: [{kind: team, name: t}]}", "wire: doc 0 kind=PolicyBinding name=b: PolicyBinding no longer has spec.subjects (use accounts (account names))"},
		{"kind: PolicyBinding\nmetadata: {name: b}\nspec: {policy: p, project: x, accounts: [a]}", "wire: doc 0 kind=PolicyBinding name=b: PolicyBinding no longer has spec.project (use metadata.owner)"},
		{"kind: ServiceAccount\nmetadata: {name: sa}\nspec: {project: p, policy: x}", "wire: doc 0 kind=ServiceAccount name=sa: ServiceAccount no longer has spec.policy (use a PolicyBinding on the service account's account)"},
		{"kind: HostKey\nmetadata: {name: k}\nspec: {hostId: h, policyId: p}", "wire: doc 0 kind=HostKey name=k: HostKey no longer has spec.policyId (use account, plus a PolicyBinding of the account's tier policy)"},
		{"kind: RelayKey\nmetadata: {name: legacy}\nspec: {policy: p}", `wire: doc 0 kind=RelayKey name=legacy: unknown kind "RelayKey"`},
	} {
		_, err := manifest.Parse(strings.NewReader("apiVersion: relay.wyolet.dev/v1alpha3\n" + tc.doc))
		if got := errText(err); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.doc, got, tc.want)
		}
	}
}
