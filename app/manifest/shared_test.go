package manifest_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
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
	rev.Projects = map[string]string{"proj-ml": "ml"}
	rev.ServiceAccounts = map[string]string{"sa-ci": "ci"}

	docs, err := manifest.Parse(strings.NewReader(sharedYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a, err := manifest.ToAccount(*docs[0].Account, idx)
	if err != nil {
		t.Fatalf("ToAccount: %v", err)
	}
	a.Meta.ID = "acc-share"
	if a.Spec.SourceAccountID != "acc-mmm" || a.Spec.Holder == nil || *a.Spec.Holder != (account.Holder{Kind: account.HolderServiceAccount, ID: "sa-ci"}) || a.Meta.Owner != (meta.Owner{Kind: meta.OwnerProject, ID: "proj-ml"}) {
		t.Fatalf("share = %+v %+v", a.Meta.Owner, a.Spec)
	}
	if k := a.DerivedKind(); k != account.KindProvider {
		t.Fatalf("kind %q, want a provider account", k)
	}
	if back := manifest.FromAccount(a, rev); back.Spec.SourceAccount != "acme-anthropic" || back.Spec.ServiceAccount != "ci" || back.Spec.Host != "anthropic-direct" {
		t.Errorf("FromAccount = %+v", back.Spec)
	}

	p, err := manifest.ToPool(*docs[1].Pool, idx)
	if err != nil {
		t.Fatalf("ToPool: %v", err)
	}
	if !reflect.DeepEqual(p.Spec.AccountIDs, []string{"acc-share", "acc-mmm"}) || !reflect.DeepEqual(p.Spec.CredentialIDs, []string{"hk-ggg"}) {
		t.Fatalf("pool spec = %+v", p.Spec)
	}
	if back := manifest.FromPool(p, rev); !reflect.DeepEqual(back.Spec.Accounts, []string{"acme-for-ci", "acme-anthropic"}) {
		t.Errorf("FromPool accounts = %v", back.Spec.Accounts)
	}

	missing := *docs[0].Account
	missing.Spec.SourceAccount = "gone"
	if _, err := manifest.ToAccount(missing, idx); err == nil {
		t.Error("a share of an unknown account was accepted")
	}
}
