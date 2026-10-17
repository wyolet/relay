//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Pruning a service account cascades through the foreign keys to its keys and its subject rows on bindings. Apply refuses while any of them stays out of the prune, and a key document naming the account is refused before it is written.
func TestIntegration_ApplyPruneOfAServiceAccountRefusesSilentCascades(t *testing.T) {
	t.Parallel()
	st := newStack(t)
	ctx := context.Background()
	seedBuiltinRoles(t, st)
	const header = "apiVersion: relay.wyolet.dev/v1alpha2\n"
	kept := header + "kind: Team\nmetadata: {name: alpha, owner: {kind: system}}\nspec: {}\n" +
		"---\n" + header + "kind: Project\nmetadata: {name: search}\nspec: {team: alpha}\n" +
		"---\n" + header + "kind: Policy\nmetadata: {name: search-policy, owner: {kind: project, name: search}}\nspec: {}\n"
	account := "---\n" + header + "kind: ServiceAccount\nmetadata: {name: indexer, labels: {managed: gitops}}\nspec: {project: search}\n"
	dependents := func(keyLabels, rbLabels, pbLabels string) string {
		return "---\n" + header + "kind: Key\nmetadata: {name: indexer-key, owner: {kind: project, name: search}" + keyLabels + "}\n" +
			"spec: {principal: {kind: serviceaccount, name: indexer}, keyHash: " + strings.Repeat("a", 64) + "}\n" +
			"---\n" + header + "kind: RoleBinding\nmetadata: {name: indexer-readers, owner: {kind: project, name: search}" + rbLabels + "}\n" +
			"spec: {role: viewer, scope: {kind: project, name: search}, subjects: [{kind: serviceaccount, name: indexer}]}\n" +
			"---\n" + header + "kind: PolicyBinding\nmetadata: {name: indexer-policy, owner: {kind: project, name: search}" + pbLabels + "}\n" +
			"spec: {project: search, policy: search-policy, subjects: [{kind: serviceaccount, name: indexer}]}\n"
	}
	const selected = ", labels: {managed: gitops}"
	const prune = "prune=true&selector=managed=gitops"
	if code, _, raw := st.applyBundle(kept+account+dependents("", "", ""), ""); code != http.StatusOK {
		t.Fatalf("seed bundle: %d %s", code, raw)
	}
	accountExists := func() bool {
		sas, err := st.stores.ServiceAccount.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(sas) == 1
	}

	// Dropping the account from the bundle would take its key and binding subjects along; dry run and apply both refuse, naming each.
	for _, q := range []string{prune + "&dryRun=true", prune} {
		code, _, raw := st.applyBundle(kept, q)
		if code != http.StatusBadRequest {
			t.Fatalf("prune %s = %d, want 400: %s", q, code, raw)
		}
		for _, row := range []string{"Key/indexer-key", "RoleBinding/indexer-readers", "PolicyBinding/indexer-policy"} {
			if !strings.Contains(string(raw), row) {
				t.Errorf("prune %s refusal does not name %s: %s", q, row, raw)
			}
		}
	}
	if !accountExists() {
		t.Fatal("the refused prune deleted the service account")
	}

	// Each dependent stops blocking once the prune selects it too.
	for _, step := range []struct {
		labels   [3]string
		blocking []string
	}{
		{[3]string{selected, "", ""}, []string{"RoleBinding/indexer-readers", "PolicyBinding/indexer-policy"}},
		{[3]string{selected, selected, ""}, []string{"PolicyBinding/indexer-policy"}},
	} {
		if code, _, raw := st.applyBundle(kept+account+dependents(step.labels[0], step.labels[1], step.labels[2]), ""); code != http.StatusOK {
			t.Fatalf("relabel: %d %s", code, raw)
		}
		code, _, raw := st.applyBundle(kept, prune)
		if code != http.StatusBadRequest {
			t.Fatalf("prune with %v still unselected = %d, want 400: %s", step.blocking, code, raw)
		}
		if strings.Contains(string(raw), "Key/indexer-key") {
			t.Errorf("a selected key still blocks the prune: %s", raw)
		}
		for _, row := range step.blocking {
			if !strings.Contains(string(raw), row) {
				t.Errorf("refusal does not name %s: %s", row, raw)
			}
		}
	}
	if code, _, raw := st.applyBundle(kept+account+dependents(selected, selected, selected), ""); code != http.StatusOK {
		t.Fatalf("relabel: %d %s", code, raw)
	}

	// A new key for the pruned account would be written and cascaded away.
	newKey := "---\n" + header + "kind: Key\nmetadata: {name: crawler-key, owner: {kind: project, name: search}}\n" +
		"spec: {principal: {kind: serviceaccount, name: indexer}, keyHash: " + strings.Repeat("b", 64) + "}\n"
	if code, _, raw := st.applyBundle(kept+newKey, prune+"&dryRun=true"); code != http.StatusBadRequest || !strings.Contains(string(raw), "its service account is pruned in the same apply") {
		t.Fatalf("key of a pruned account = %d, want 400: %s", code, raw)
	}

	code, plan, raw := st.applyBundle(kept, prune)
	if code != http.StatusOK {
		t.Fatalf("prune with every dependent selected = %d: %s", code, raw)
	}
	for kind, name := range map[string]string{"ServiceAccount": "indexer", "Key": "indexer-key", "RoleBinding": "indexer-readers", "PolicyBinding": "indexer-policy"} {
		if got := plan.action(kind, name); got != "delete" {
			t.Errorf("%s/%s action = %q, want delete", kind, name, got)
		}
	}
	if accountExists() {
		t.Fatal("the service account survived its prune")
	}
}
