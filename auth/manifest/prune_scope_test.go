package manifest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/auth/manifest"
)

// Pruning a scope deletes its bindings, so a bundle that keeps a binding while pruning its project, or that project's team, is refused rather than written and then lost.
func TestPlanRefusesABindingWhoseScopeIsPruned(t *testing.T) {
	e := newEnv(t)
	e.apply(t, bundle, manifest.Options{})
	docs := strings.Split(bundle, "---\n")
	prune := manifest.Options{Prune: true, Selector: map[string]string{"managed-by": "gitops"}, Refs: e.opts.Refs, Names: e.opts.Names}
	for name, kept := range map[string][]string{
		"project pruned":          append([]string{docs[0]}, docs[2:]...),
		"team and project pruned": docs[2:],
	} {
		_, err := e.reg.Plan(context.Background(), e.store.Stores(), e.parse(t, strings.Join(kept, "---\n")), prune)
		if !errors.Is(err, manifest.ErrScopePruned) {
			t.Fatalf("%s: plan err = %v, want ErrScopePruned", name, err)
		}
		if got := asError(t, err); got.Kind != manifest.KindRoleBinding || got.Name != "search-readers" || got.Field != "spec.scope" {
			t.Fatalf("%s: error on %s %q %s, want the binding's scope", name, got.Kind, got.Name, got.Field)
		}
	}
	// Dropping the binding and the service account too is a plain prune.
	p := e.plan(t, e.parse(t, strings.Join(docs[3:len(docs)-1], "---\n")), prune)
	if !strings.Contains(strings.Join(actions(p), "\n"), "delete RoleBinding/search-readers") {
		t.Fatalf("prune without the binding = %v", actions(p))
	}
}

// Pruning a project deletes its service accounts, so a bundle that keeps or adds one there while pruning the project, or its team, is refused too.
func TestPlanRefusesAServiceAccountWhoseProjectIsPruned(t *testing.T) {
	e := newEnv(t)
	e.apply(t, bundle, manifest.Options{})
	docs := strings.Split(bundle, "---\n")
	prune := manifest.Options{Prune: true, Selector: map[string]string{"managed-by": "gitops"}, Refs: e.opts.Refs, Names: e.opts.Names}
	crawler := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: ServiceAccount\nmetadata: {name: crawler, labels: {managed-by: gitops}}\nspec: {project: search}\n"
	rest := docs[3 : len(docs)-1]
	for name, c := range map[string]struct {
		kept    []string
		account string
	}{
		"project pruned":                       {append([]string{docs[0], docs[2]}, rest...), "indexer"},
		"team and project pruned":              {append([]string{docs[2]}, rest...), "indexer"},
		"new account, project pruned":          {append([]string{docs[0], crawler}, rest...), "crawler"},
		"new account, team and project pruned": {append([]string{crawler}, rest...), "crawler"},
	} {
		_, err := e.reg.Plan(context.Background(), e.store.Stores(), e.parse(t, strings.Join(c.kept, "---\n")), prune)
		if !errors.Is(err, manifest.ErrScopePruned) {
			t.Fatalf("%s: plan err = %v, want ErrScopePruned", name, err)
		}
		if got := asError(t, err); got.Kind != manifest.KindServiceAccount || got.Name != c.account || got.Field != "spec.project" {
			t.Fatalf("%s: error on %s %q %s, want the account's project", name, got.Kind, got.Name, got.Field)
		}
	}
}
