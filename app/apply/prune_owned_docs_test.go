package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	authmanifest "github.com/wyolet/relay/auth/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/meta"
)

const docHeader = "apiVersion: relay.wyolet.dev/v1alpha3\n"

// prunedScopeRows is tenancyRows with its service account pruned as well; the run prunes the project and, when teamPruned, its team.
func prunedScopeRows(teamPruned bool) *Rows {
	rows := tenancyRows(true)
	if !teamPruned {
		rows.Teams[0].Meta.Labels = nil
	}
	return rows
}

// planPruned runs a pruning plan of rows over the one YAML document.
func planPruned(t *testing.T, rows *Rows, doc string) error {
	t.Helper()
	return pruneAll(rows).run(context.Background(), userDocs(t, docHeader+doc))
}

func wantRefused(t *testing.T, err error, kind, name string, want error) {
	t.Helper()
	var invalid *InvalidError
	if !errors.As(err, &invalid) || invalid.Kind != kind || invalid.Name != name || !errors.Is(err, want) {
		t.Fatalf("err = %v, want %s/%s refused with %q", err, kind, name, want)
	}
}

// The team's delete cascades to its projects, a new one included.
func TestPruneRefusesAProjectInAPrunedTeam(t *testing.T) {
	err := planPruned(t, prunedScopeRows(true), "kind: Project\nmetadata: {name: ml-new}\nspec: {team: platform}\n")
	wantRefused(t, err, "Project", "ml-new", authmanifest.ErrScopePruned)
}

// A key goes with its service account, which goes with the account's project.
func TestPruneRefusesAKeyInAPrunedProject(t *testing.T) {
	doc := "kind: Key\nmetadata: {name: indexer-key, owner: {kind: project, name: ml-search}}\n" +
		"spec: {principal: {kind: serviceaccount, name: indexer}, keyHash: " + strings.Repeat("a", 64) + "}\n"
	for name, teamPruned := range map[string]bool{"project pruned": false, "team and project pruned": true} {
		t.Run(name, func(t *testing.T) {
			wantRefused(t, planPruned(t, prunedScopeRows(teamPruned), doc), "Key", "indexer-key", authmanifest.ErrScopePruned)
		})
	}
}

// A binding owned by the project goes with it.
func TestPruneRefusesAPolicyBindingInAPrunedProject(t *testing.T) {
	rows := prunedScopeRows(false)
	acct := account.ForProject(rows.Projects[0].Meta.ID, rows.Projects[0].Meta.Name, "")
	acct.Meta.ID = meta.NewID()
	rows.Accounts = []*account.Account{acct}
	rows.Policies = []*policy.Policy{{Meta: meta.Metadata{ID: meta.NewID(), Name: "shared-policy", Owner: meta.Owner{Kind: meta.OwnerSystem}}}}
	doc := "kind: PolicyBinding\nmetadata: {name: ml-binding, owner: {kind: project, name: ml-search}}\n" +
		"spec: {policy: shared-policy, accounts: [" + acct.Meta.Name + "]}\n"
	wantRefused(t, planPruned(t, rows, doc), "PolicyBinding", "ml-binding", authmanifest.ErrScopePruned)
}

// Nothing deletes a policy with its project, so it would be left owned by a project that no longer exists.
func TestPruneRefusesAPolicyOwnedByAPrunedProject(t *testing.T) {
	doc := "kind: Policy\nmetadata: {name: ml-policy, owner: {kind: project, name: ml-search}}\nspec: {}\n"
	wantRefused(t, planPruned(t, prunedScopeRows(false), doc), "Policy", "ml-policy", errOwnerPruned)

	// The same document is fine while its project stays.
	rows := prunedScopeRows(false)
	rows.Projects[0].Meta.Labels = nil
	if err := planPruned(t, rows, doc); err != nil {
		t.Fatalf("a policy in a kept project was refused: %v", err)
	}
}

func TestPruneRefusesAHostKeyOwnedByAPrunedProject(t *testing.T) {
	rows := prunedScopeRows(false)
	h := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-api", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://acme.example.com"}}
	rows.Hosts = []*host.Host{h}
	rows.Accounts = []*account.Account{{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-org", Owner: meta.Owner{Kind: meta.OwnerProject, ID: rows.Projects[0].Meta.ID}, Labels: pruneLabels}, Spec: account.Spec{HostID: h.Meta.ID}}}
	doc := "kind: HostKey\nmetadata: {name: ml-key, owner: {kind: project, name: ml-search}}\n" +
		"spec: {hostId: acme-api, account: ml-org, valueFrom: {kind: env, env: ML_KEY}}\n"
	wantRefused(t, planPruned(t, rows, doc), "HostKey", "ml-key", errOwnerPruned)
}

// Accounts, pools and budgets outlive their project the same way.
func TestPruneRefusesUpstreamRowsOwnedByAPrunedProject(t *testing.T) {
	h := &host.Host{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-api", Owner: meta.Owner{Kind: meta.OwnerSystem}}, Spec: host.Spec{BaseURL: "https://acme.example.com"}}
	for kind, doc := range map[string]string{
		"Account": "kind: Account\nmetadata: {name: ml-acct, owner: {kind: project, name: ml-search}}\nspec: {host: acme-api}\n",
		"Pool":    "kind: Pool\nmetadata: {name: ml-pool, owner: {kind: project, name: ml-search}}\nspec: {host: acme-api}\n",
		"Budget":  "kind: Budget\nmetadata: {name: ml-budget, owner: {kind: project, name: ml-search}}\nspec: {amountUsd: \"10\"}\n",
	} {
		t.Run(kind, func(t *testing.T) {
			rows := prunedScopeRows(false)
			rows.Hosts = []*host.Host{h}
			err := planPruned(t, rows, doc)
			var invalid *InvalidError
			if !errors.As(err, &invalid) || invalid.Kind != kind || !errors.Is(err, errOwnerPruned) {
				t.Fatalf("err = %v, want %s refused with %q", err, kind, errOwnerPruned)
			}
		})
	}
}

// A rate limit cannot be team-owned; pruning the team still takes its project's.
func TestPruneRefusesARateLimitOwnedByAProjectOfAPrunedTeam(t *testing.T) {
	doc := "kind: RateLimit\nmetadata: {name: ml-burst, owner: {kind: project, name: ml-search}}\n" +
		"spec: {rules: [{meter: requests, amount: 10, window: 1m, strategy: token-bucket}]}\n"
	wantRefused(t, planPruned(t, prunedScopeRows(true), doc), "RateLimit", "ml-burst", errOwnerPruned)
}
