package apply

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/policybinding"
	apppool "github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	authmanifest "github.com/wyolet/relay/auth/manifest"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

var pruneLabels = map[string]string{"bundle": "ml"}

// tenancyRows is a team holding one project holding one service account;
// saLabelled decides whether the account matches the prune selector too.
func tenancyRows(saLabelled bool) *Rows {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}, Labels: pruneLabels}}
	proj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search", Labels: pruneLabels}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	proj.StampOwner()
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"}, Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID}}
	sa.StampOwner()
	if saLabelled {
		sa.Meta.Labels = pruneLabels
	}
	return &Rows{Teams: []*team.Team{tm}, Projects: []*project.Project{proj}, ServiceAccounts: []*serviceaccount.ServiceAccount{sa}}
}

func pruneAll(rows *Rows) *builder {
	return &builder{
		opts: Options{Stores: &Stores{}, Prune: true}, rows: rows, idx: newIndex(rows),
		selector: labelSelector(pruneLabels), admin: true,
	}
}

// Pruning a project the foreign keys would cascade from — taking its
// service accounts and their keys along unannounced — is refused before
// anything is written, naming what is in the way.
func TestPruneRefusesAProjectThatStillHasRows(t *testing.T) {
	b := pruneAll(tenancyRows(false))
	err := b.run(context.Background(), nil)
	want := `apply: Project "ml-search": still has ServiceAccount/indexer: delete or move them first`
	if err == nil || err.Error() != want || len(b.entries) != 0 {
		t.Fatalf("err = %v (entries %d), want %q", err, len(b.entries), want)
	}
}

// A new service account in a project the same run prunes, alone or with its team, is refused while planning: the cascade would delete it right after the write.
func TestPruneRefusesAServiceAccountInAPrunedProject(t *testing.T) {
	doc := userDocs(t, "apiVersion: relay.wyolet.dev/v1alpha2\nkind: ServiceAccount\nmetadata: {name: crawler}\nspec: {project: ml-search}\n")
	for name, teamPruned := range map[string]bool{"project pruned": false, "team and project pruned": true} {
		rows := tenancyRows(true)
		rows.ServiceAccounts = nil
		if !teamPruned {
			rows.Teams[0].Meta.Labels = nil
		}
		b := pruneAll(rows)
		err := b.run(context.Background(), doc)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || invalid.Kind != "ServiceAccount" || invalid.Name != "crawler" || !errors.Is(err, authmanifest.ErrScopePruned) {
			t.Fatalf("%s: err = %v, want ServiceAccount/crawler refused with ErrScopePruned", name, err)
		}
	}
}

// Rows the same run prunes are deleted explicitly, children first, so they
// do not block their parent.
func TestPruneDeletesATenancyTreeItRemovesWhole(t *testing.T) {
	b := pruneAll(tenancyRows(true))
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	var order []string
	for _, e := range b.entries {
		if e.Action == ActionDelete {
			order = append(order, e.Kind)
		}
	}
	if got := strings.Join(order, ","); got != "ServiceAccount,Project,Team" {
		t.Fatalf("delete order = %s, want children before parents", got)
	}
}

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

// accountRows is tenancyRows with only the service account selected: its team and project stay.
func accountRows() (*Rows, *serviceaccount.ServiceAccount) {
	rows := tenancyRows(true)
	rows.Teams[0].Meta.Labels = nil
	rows.Projects[0].Meta.Labels = nil
	rows.Roles = []*role.Role{{Meta: meta.Metadata{ID: meta.NewID(), Name: "viewer", Owner: meta.Owner{Kind: meta.OwnerSystem}}}}
	return rows, rows.ServiceAccounts[0]
}

func accountKey(sa *serviceaccount.ServiceAccount, labels map[string]string) *key.Key {
	return &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-key", Owner: sa.Meta.Owner, Labels: labels},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}, KeyHash: strings.Repeat("a", 64)},
	}
}

func accountSubjects(sa *serviceaccount.ServiceAccount) []rolebinding.Subject {
	return []rolebinding.Subject{{Kind: rolebinding.SubjectServiceAccount, ID: sa.Meta.ID}, {Kind: rolebinding.SubjectGroup, Name: "eng"}}
}

func accountRoleBinding(rows *Rows, sa *serviceaccount.ServiceAccount, labels map[string]string) *rolebinding.RoleBinding {
	scope := meta.Owner{Kind: meta.OwnerProject, ID: rows.Projects[0].Meta.ID}
	rb := &rolebinding.RoleBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-readers", Owner: scope, Labels: labels}}
	rb.Spec.RoleID = rows.Roles[0].Meta.ID
	rb.Spec.Scope = scope
	rb.Spec.Subjects = accountSubjects(sa)
	return rb
}

func accountPolicyBinding(rows *Rows, sa *serviceaccount.ServiceAccount, labels map[string]string) *policybinding.PolicyBinding {
	pol := &policy.Policy{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-policy", Owner: sa.Meta.Owner}}
	rows.Policies = append(rows.Policies, pol)
	acct := account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, "", sa.Meta.Owner)
	acct.Meta.ID = meta.NewID()
	rows.Accounts = append(rows.Accounts, acct)
	pb := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-policy", Owner: sa.Meta.Owner, Labels: labels}}
	pb.Spec.PolicyID = pol.Meta.ID
	pb.Spec.AccountIDs = []string{acct.Meta.ID}
	return pb
}

// wantAccountBlocked asserts the service account's prune is refused, naming the row in the way.
func wantAccountBlocked(t *testing.T, err error, row string) {
	t.Helper()
	var invalid *InvalidError
	var deps *project.DependentsError
	if !errors.As(err, &invalid) || invalid.Kind != "ServiceAccount" || invalid.Name != "indexer" || !errors.As(err, &deps) || !strings.Contains(err.Error(), row) {
		t.Fatalf("err = %v, want ServiceAccount/indexer refused naming %s", err, row)
	}
}

func deleteOrder(b *builder) string {
	var order []string
	for _, e := range b.entries {
		if e.Action == ActionDelete {
			order = append(order, e.Kind)
		}
	}
	return strings.Join(order, ",")
}

// A key goes with its service account, so a key document naming an account the same run prunes would be written and then lost, even while the project stays.
func TestPruneRefusesAKeyOfAPrunedServiceAccount(t *testing.T) {
	rows, _ := accountRows()
	doc := "kind: Key\nmetadata: {name: crawler-key, owner: {kind: project, name: ml-search}}\n" +
		"spec: {principal: {kind: serviceaccount, name: indexer}, keyHash: " + strings.Repeat("b", 64) + "}\n"
	wantRefused(t, planPruned(t, rows, doc), "Key", "crawler-key", errPrincipalPruned)
}

// Pruning a service account would cascade to its keys; one the selector leaves out blocks the prune instead of disappearing unannounced.
func TestPruneRefusesAServiceAccountWithUnselectedKeys(t *testing.T) {
	rows, sa := accountRows()
	rows.Keys = []*key.Key{accountKey(sa, nil)}
	wantAccountBlocked(t, pruneAll(rows).run(context.Background(), nil), "Key/indexer-key")

	rows, sa = accountRows()
	rows.Keys = []*key.Key{accountKey(sa, pruneLabels)}
	b := pruneAll(rows)
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("pruning the account with its key: %v", err)
	}
	if got := deleteOrder(b); got != "Key,ServiceAccount" {
		t.Fatalf("delete order = %s, want the key before its account", got)
	}
}

// A binding naming the service account would silently lose that subject when the account is deleted, so it blocks the prune unless it is pruned too.
func TestPruneRefusesAServiceAccountStillBound(t *testing.T) {
	for _, tc := range []struct {
		kind string
		add  func(rows *Rows, sa *serviceaccount.ServiceAccount, labels map[string]string)
	}{
		{"RoleBinding", func(rows *Rows, sa *serviceaccount.ServiceAccount, labels map[string]string) {
			rows.RoleBindings = []*rolebinding.RoleBinding{accountRoleBinding(rows, sa, labels)}
		}},
		{"PolicyBinding", func(rows *Rows, sa *serviceaccount.ServiceAccount, labels map[string]string) {
			rows.PolicyBindings = []*policybinding.PolicyBinding{accountPolicyBinding(rows, sa, labels)}
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			rows, sa := accountRows()
			tc.add(rows, sa, nil)
			wantAccountBlocked(t, pruneAll(rows).run(context.Background(), nil), tc.kind+"/indexer-")

			rows, sa = accountRows()
			tc.add(rows, sa, pruneLabels)
			if err := pruneAll(rows).run(context.Background(), nil); err != nil {
				t.Fatalf("pruning the account with its binding: %v", err)
			}
		})
	}
}

// The run's writes land before its deletes, so a binding is judged as the run leaves it: one updated to drop the account no longer blocks, one created naming it does.
func TestPruneOfAServiceAccountReadsBindingsAfterTheRun(t *testing.T) {
	rows, sa := accountRows()
	rows.RoleBindings = []*rolebinding.RoleBinding{accountRoleBinding(rows, sa, nil)}
	dropped := "kind: RoleBinding\nmetadata: {name: indexer-readers, owner: {kind: project, name: ml-search}}\n" +
		"spec: {role: viewer, scope: {kind: project, name: ml-search}, subjects: [{kind: group, name: eng}]}\n"
	if err := planPruned(t, rows, dropped); err != nil {
		t.Fatalf("a binding updated to drop the account still blocked its prune: %v", err)
	}

	rows, _ = accountRows()
	created := "kind: RoleBinding\nmetadata: {name: crawler-readers, owner: {kind: project, name: ml-search}}\n" +
		"spec: {role: viewer, scope: {kind: project, name: ml-search}, subjects: [{kind: serviceaccount, name: indexer}]}\n"
	wantAccountBlocked(t, planPruned(t, rows, created), "RoleBinding/crawler-readers")
}

// teamOwnedRows is a pruned team without projects that owns one row of every kind whose owner lives only in metadata; ownedLabelled decides whether the owned rows match the prune selector too.
func teamOwnedRows(ownedLabelled bool) *Rows {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}, Labels: pruneLabels}}
	owner := meta.Owner{Kind: meta.OwnerTeam, ID: tm.Meta.ID}
	md := func(name string) meta.Metadata {
		m := meta.Metadata{ID: meta.NewID(), Name: name, Owner: owner}
		if ownedLabelled {
			m.Labels = pruneLabels
		}
		return m
	}
	hostID := meta.NewID()
	named := &account.Account{Meta: md("team-account"), Spec: account.Spec{HostID: hostID}}
	return &Rows{
		Teams:          []*team.Team{tm},
		Policies:       []*policy.Policy{{Meta: md("team-policy")}},
		HostKeys:       []*hostkey.HostKey{{Meta: md("team-key"), Spec: hostkey.Spec{HostID: hostID, AccountID: named.Meta.ID}}},
		RateLimits:     []*ratelimit.RateLimit{{Meta: md("team-limit")}},
		Accounts:       []*account.Account{named},
		Pools:          []*apppool.Pool{{Meta: md("team-pool"), Spec: apppool.Spec{HostID: hostID}}},
		Budgets:        []*budget.Budget{{Meta: md("team-budget"), Spec: budget.Spec{AmountUSD: "10"}}},
		PolicyBindings: []*policybinding.PolicyBinding{{Meta: md("team-binding")}},
	}
}

// A team's prune is refused while it owns rows its delete would orphan or cascade to, as the control API's delete is.
func TestPruneRefusesATeamThatStillOwnsRows(t *testing.T) {
	err := pruneAll(teamOwnedRows(false)).run(context.Background(), nil)
	var invalid *InvalidError
	var deps *project.DependentsError
	if !errors.As(err, &invalid) || invalid.Kind != "Team" || !errors.As(err, &deps) {
		t.Fatalf("err = %v, want the team refused naming its rows", err)
	}
	want := []string{"Policy/team-policy", "HostKey/team-key", "RateLimit/team-limit", "Account/team-account", "Pool/team-pool", "Budget/team-budget", "PolicyBinding/team-binding"}
	if strings.Join(deps.Rows, ",") != strings.Join(want, ",") || deps.Hidden != 0 {
		t.Fatalf("blocking rows = %v (+%d hidden), want %v", deps.Rows, deps.Hidden, want)
	}
	if !strings.Contains(err.Error(), "delete or move them first") {
		t.Fatalf("err = %v, want the dependents message", err)
	}
}

// Rows the same run prunes do not block their team.
func TestPruneDeletesATeamWithTheRowsItOwns(t *testing.T) {
	b := pruneAll(teamOwnedRows(true))
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	var deleted []string
	for _, e := range b.entries {
		if e.Action == ActionDelete {
			deleted = append(deleted, e.Kind+"/"+e.Name)
		}
	}
	want := []string{"PolicyBinding/team-binding", "Policy/team-policy", "Pool/team-pool", "HostKey/team-key",
		"Budget/team-budget", "RateLimit/team-limit", "Account/team-account", "Team/platform"}
	if !slices.Equal(deleted, want) {
		t.Fatalf("deletes = %v, want %v (dependents before what they reference, the team last)", deleted, want)
	}
}

func entryFor(t *testing.T, b *builder, kind, name string) Entry {
	t.Helper()
	for _, e := range b.entries {
		if e.Kind == kind && e.Name == name {
			return e
		}
	}
	t.Fatalf("no %s %q in the plan", kind, name)
	return Entry{}
}

// rotatedKeyRows is a service account key mid-rotation: the old hash still
// authenticates until graceUntil.
func rotatedKeyRows() (*Rows, *key.Key) {
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search"}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	proj.StampOwner()
	sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"}, Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID}}
	sa.StampOwner()
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-key", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}},
		Spec: key.Spec{
			Principal:       key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID},
			KeyHash:         strings.Repeat("a", 64),
			PreviousKeyHash: strings.Repeat("b", 64),
			GraceUntil:      &until,
		},
	}
	return &Rows{
		Teams: []*team.Team{tm}, Projects: []*project.Project{proj},
		ServiceAccounts: []*serviceaccount.ServiceAccount{sa}, Keys: []*key.Key{k},
	}, k
}

func keyDoc(k *key.Key) *manifest.KeyDTO {
	d := &manifest.KeyDTO{APIVersion: manifest.APIVersion, Kind: "Key"}
	d.Metadata.Name = k.Meta.Name
	d.Metadata.Owner = manifest.WireOwner{Kind: meta.OwnerProject, Name: "ml-search"}
	d.Spec.Principal = manifest.PrincipalDTO{Kind: "serviceaccount", Name: "indexer"}
	d.Spec.KeyHash = k.Spec.KeyHash
	return d
}

// The rotation grace window is server state a manifest cannot author:
// re-applying an exported key is not a change, and an edit keeps it.
func TestApplyLeavesKeyRotationStateAlone(t *testing.T) {
	rows, k := rotatedKeyRows()
	b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
	if err := b.run(context.Background(), []manifest.Document{{Key: keyDoc(k)}}); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if e := entryFor(t, b, "Key", k.Meta.Name); e.Action != ActionUnchanged {
		t.Fatalf("action = %s (fields %v), want unchanged", e.Action, e.ChangedFields)
	}

	next := &key.Key{Spec: key.Spec{KeyHash: k.Spec.KeyHash}}
	if err := keepKeyServerFields(k, next); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if next.Spec.PreviousKeyHash != k.Spec.PreviousKeyHash || next.Spec.GraceUntil != k.Spec.GraceUntil {
		t.Fatalf("update dropped the rotation state: %+v", next.Spec)
	}
}

// A key's hash, prefix and principal change only through the control API: a document declaring another value is refused, one omitting them keeps the stored row's.
func TestApplyRefusesChangingKeyIdentity(t *testing.T) {
	rows, k := rotatedKeyRows()
	k.Spec.Prefix = "rk_aaaa"
	other := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "crawler"}, Spec: serviceaccount.Spec{ProjectID: rows.Projects[0].Meta.ID}}
	other.StampOwner()
	rows.ServiceAccounts = append(rows.ServiceAccounts, other)

	cases := map[string]func(*manifest.KeyDTO){
		"keyHash":   func(d *manifest.KeyDTO) { d.Spec.KeyHash = strings.Repeat("c", 64) },
		"prefix":    func(d *manifest.KeyDTO) { d.Spec.Prefix = "rk_cccc" },
		"principal": func(d *manifest.KeyDTO) { d.Spec.Principal.Name = "crawler" },
	}
	for field, edit := range cases {
		t.Run(field, func(t *testing.T) {
			doc := keyDoc(k)
			edit(doc)
			b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows), admin: true}
			err := b.run(context.Background(), []manifest.Document{{Key: doc}})
			var ie *InvalidError
			if !errors.As(err, &ie) || !strings.Contains(err.Error(), "spec."+field) {
				t.Fatalf("plan: %v, want an InvalidError naming spec.%s", err, field)
			}
		})
	}

	doc := keyDoc(k)
	doc.Spec.KeyHash = ""
	doc.Spec.Enabled = new(false)
	b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
	if err := b.run(context.Background(), []manifest.Document{{Key: doc}}); err != nil {
		t.Fatalf("plan without keyHash: %v", err)
	}
	if e := entryFor(t, b, "Key", k.Meta.Name); e.Action != ActionUpdate || slices.Contains(e.ChangedFields, "spec.keyHash") {
		t.Fatalf("action = %s (fields %v), want an update that leaves the hash", e.Action, e.ChangedFields)
	}
	if got := b.declared[k.Meta.ID].(*key.Key).Spec; got.KeyHash != k.Spec.KeyHash || got.Prefix != k.Spec.Prefix || got.Principal != k.Spec.Principal {
		t.Fatalf("update wrote %+v, want the stored hash, prefix and principal", got)
	}
}

// Whoever picks a key's hash knows its plaintext, so creating a key from a document is an admin's: the control API's create never takes a hash.
func TestApplyKeyCreateFromHashIsAdminOnly(t *testing.T) {
	for _, admin := range []bool{false, true} {
		rows, k := rotatedKeyRows()
		rows.Keys = nil
		b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows), admin: admin}
		err := b.run(context.Background(), []manifest.Document{{Key: keyDoc(k)}})
		if !admin {
			if !errors.Is(err, authz.ErrForbidden) {
				t.Fatalf("non-admin create: %v, want forbidden", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("admin create: %v", err)
		}
		if e := entryFor(t, b, "Key", k.Meta.Name); e.Action != ActionCreate {
			t.Fatalf("admin create: action = %s", e.Action)
		}
	}
}
