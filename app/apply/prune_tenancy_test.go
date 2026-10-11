package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	authmanifest "github.com/wyolet/relay/auth/manifest"
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
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want a plan error", err)
	}
	if invalid.Kind != "Project" || !strings.Contains(err.Error(), "ServiceAccount/indexer") {
		t.Fatalf("err = %v, want the project refused naming its service account", err)
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
