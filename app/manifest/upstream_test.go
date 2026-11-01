package manifest_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/rolebinding"
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

// Every policy-binding subject kind resolves by name and renders back.
func TestPolicyBinding_EverySubjectKind(t *testing.T) {
	const doc = `
apiVersion: relay.wyolet.dev/v1alpha3
kind: PolicyBinding
metadata: {name: tiers, owner: {kind: system}}
spec:
  policy: cheap-tier
  subjects:
    - {kind: host, name: anthropic-direct}
    - {kind: account, name: acme-anthropic}
    - {kind: credential, name: openai-key-1}
    - {kind: team, name: platform}
    - {kind: project, name: ml-search}
    - {kind: serviceaccount, name: search-indexer}
    - {kind: user, name: alice}
    - {kind: group, name: eng}
`
	idx := testResolver
	idx.Teams = map[string]string{"platform": teamUUID}
	idx.Projects = map[string]string{"ml-search": projectUUID}
	idx.ServiceAccounts = map[string]string{"search-indexer": saUUID}
	idx.Users = map[string]string{"alice": aliceUUID}
	rev := testRev
	rev.Teams = map[string]string{teamUUID: "platform"}
	rev.Projects = map[string]string{projectUUID: "ml-search"}
	rev.ServiceAccounts = map[string]string{saUUID: "search-indexer"}
	rev.Users = map[string]string{aliceUUID: "alice"}

	docs, err := manifest.Parse(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b, err := manifest.ToPolicyBinding(*docs[0].PolicyBinding, idx)
	if err != nil {
		t.Fatalf("ToPolicyBinding: %v", err)
	}
	want := []rolebinding.Subject{
		{Kind: policybinding.SubjectHost, ID: "host-bbb"},
		{Kind: policybinding.SubjectAccount, ID: "acc-mmm"},
		{Kind: policybinding.SubjectCredential, ID: "hk-ggg"},
		{Kind: policybinding.SubjectTeam, ID: teamUUID},
		{Kind: policybinding.SubjectProject, ID: projectUUID},
		{Kind: rolebinding.SubjectServiceAccount, ID: saUUID},
		{Kind: rolebinding.SubjectUser, ID: aliceUUID},
		{Kind: rolebinding.SubjectGroup, Name: "eng"},
	}
	if !reflect.DeepEqual(b.Spec.Subjects, want) {
		t.Fatalf("subjects = %+v\nwant       %+v", b.Spec.Subjects, want)
	}
	back := manifest.FromPolicyBinding(b, rev)
	if !reflect.DeepEqual(back.Spec.Subjects, docs[0].PolicyBinding.Spec.Subjects) {
		t.Errorf("rendered subjects = %+v", back.Spec.Subjects)
	}
}
