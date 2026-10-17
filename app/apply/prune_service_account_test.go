package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
)

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
	pb := &policybinding.PolicyBinding{Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-policy", Owner: sa.Meta.Owner, Labels: labels}}
	pb.Spec.ProjectID = sa.Spec.ProjectID
	pb.Spec.PolicyID = pol.Meta.ID
	pb.Spec.Subjects = accountSubjects(sa)
	pb.IndexSubjects()
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
