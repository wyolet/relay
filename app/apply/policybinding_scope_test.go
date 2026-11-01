package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
)

// Without an authorizer (the seed, the CLI) apply still keeps a binding to subjects inside its owner's scope: a project binds its own service account and not another project's.
func TestApplyKeepsBindingSubjectsInsideTheOwnersScope(t *testing.T) {
	rows := tenancyRows(false)
	other := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "other"}, Spec: project.Spec{TeamID: rows.Teams[0].Meta.ID}}
	other.StampOwner()
	rows.Projects = append(rows.Projects, other)
	rows.Policies = []*policy.Policy{{Meta: meta.Metadata{ID: meta.NewID(), Name: "shared-policy", Owner: meta.Owner{Kind: meta.OwnerSystem}}}}
	plan := func(project string) error {
		b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows)}
		doc := "kind: PolicyBinding\nmetadata: {name: indexer-policy}\n" +
			"spec: {project: " + project + ", policy: shared-policy, subjects: [{kind: serviceaccount, name: indexer}]}\n"
		return b.run(context.Background(), userDocs(t, docHeader+doc))
	}
	if err := plan("ml-search"); err != nil {
		t.Fatalf("the account's own project binding it: %v", err)
	}
	if err := plan("other"); err == nil || !strings.Contains(err.Error(), "outside the scope") {
		t.Fatalf("another project binding the account = %v, want a refusal", err)
	}
}
