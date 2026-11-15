package manifest_test

import (
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pool"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/pkg/meta"
)

const sharedYAML = `
apiVersion: relay.wyolet.dev/v1alpha3
kind: Account
metadata: {name: acme-for-ci, owner: {kind: project, name: ml}}
spec: {host: anthropic-direct, sourceAccount: acme-anthropic, serviceAccount: ci}
---
apiVersion: relay.wyolet.dev/v1alpha3
kind: Pool
metadata: {name: ci-pool, owner: {kind: project, name: ml}}
spec: {host: anthropic-direct, accounts: [acme-for-ci, acme-anthropic], credentials: [openai-key-1]}
`

// A share names its real account and, for a service account's share, the service account by name; a pool names the accounts it draws on. Both come back as written.
func TestSharedAccountAndPoolAccounts_RoundTrip(t *testing.T) {
	idx := testResolver
	idx.Accounts = map[string]string{"acme-anthropic": "acc-mmm", "acme-for-ci": "acc-share"}
	idx.Projects = map[string]string{"ml": "proj-ml"}
	idx.ServiceAccounts = map[string]string{"ci": "sa-ci"}
	rev := testRev
	rev.Accounts = map[string]string{"acc-mmm": "acme-anthropic", "acc-share": "acme-for-ci"}
	rev.ServiceAccounts = map[string]string{"sa-ci": "ci"}

	docs := parseDocs(t, sharedYAML, 2)
	inProject := meta.Owner{Kind: meta.OwnerProject, ID: "proj-ml"}
	a := translated(t, "ToAccount", &account.Account{
		Meta: meta.Metadata{Name: "acme-for-ci", Owner: inProject},
		Spec: account.Spec{HostID: "host-bbb", SourceAccountID: "acc-mmm", Holder: &account.Holder{Kind: account.HolderServiceAccount, ID: "sa-ci"}},
	})(manifest.ToAccount(*docs[0].Account, idx))
	if k := a.DerivedKind(); k != account.KindProvider {
		t.Fatalf("kind %q, want a provider account", k)
	}
	// From<Kind> leaves an owner as its id; the export names it afterwards.
	wantAccount := *docs[0].Account
	wantAccount.Metadata.Owner.Name = "proj-ml"
	exported(t, "FromAccount", wantAccount, manifest.FromAccount(a, rev))

	p := translated(t, "ToPool", &pool.Pool{
		Meta: meta.Metadata{Name: "ci-pool", Owner: inProject},
		Spec: pool.Spec{HostID: "host-bbb", AccountIDs: []string{"acc-share", "acc-mmm"}, CredentialIDs: []string{"hk-ggg"}, Selection: keypool.KeySelectionPrioritized},
	})(manifest.ToPool(*docs[1].Pool, idx))
	wantPool := *docs[1].Pool
	wantPool.Metadata.Owner.Name, wantPool.Spec.Selection = "proj-ml", "prioritized"
	exported(t, "FromPool", wantPool, manifest.FromPool(p, rev))

	for _, tc := range []struct {
		name string
		edit func(*manifest.AccountSpec)
		want string
	}{
		{"real account", func(s *manifest.AccountSpec) { s.SourceAccount = "gone" }, `account "acme-for-ci": sourceAccount "gone" not found`},
		{"service account", func(s *manifest.AccountSpec) { s.ServiceAccount = "gone" }, `account "acme-for-ci": serviceAccount "gone" not found`},
	} {
		missing := *docs[0].Account
		tc.edit(&missing.Spec)
		_, err := manifest.ToAccount(missing, idx)
		if got := errText(err); got != tc.want {
			t.Errorf("a share of an unknown %s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}
