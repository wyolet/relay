package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
)

// seesOnly is a scoping authorizer that allows every verb on the rows it
// names by id, and nothing else.
type seesOnly map[string]bool

func (s seesOnly) Authorize(_ context.Context, _ string, res authz.Resource) error {
	if s[res.ID] {
		return nil
	}
	return authz.ErrForbidden
}

func (s seesOnly) Visible(ctx context.Context, kind, id string, owner meta.Owner) bool {
	return s.Authorize(ctx, kind+".get", authz.Resource{Kind: kind, ID: id, Owner: &owner}) == nil
}

func hiddenPlanBuilder(rows *Rows, a authz.Authorizer, prune bool) *builder {
	b := &builder{opts: Options{Stores: &Stores{}, Authz: a}, rows: rows, idx: newIndex(rows)}
	if prune {
		b.opts.Prune, b.opts.Selector = true, "env=prod"
		b.selector = labelSelector{"env": "prod"}
	}
	return b
}

// tenantRows is team t > project p (both env=prod) > service accounts.
func tenantRows(accounts ...string) (*Rows, *team.Team, *project.Project) {
	lbl := map[string]string{"env": "prod"}
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "t", Owner: meta.Owner{Kind: meta.OwnerSystem}, Labels: lbl}}
	p := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "p", Labels: lbl}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	p.StampOwner()
	rows := &Rows{Teams: []*team.Team{tm}, Projects: []*project.Project{p}}
	for _, name := range accounts {
		sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: name}, Spec: serviceaccount.Spec{ProjectID: p.Meta.ID}}
		sa.StampOwner()
		rows.ServiceAccounts = append(rows.ServiceAccounts, sa)
	}
	return rows, tm, p
}

func TestPlanPruneSkipsRowsTheCallerCannotSee(t *testing.T) {
	rows, _, _ := tenantRows("billing-bot")
	b := hiddenPlanBuilder(rows, seesOnly{}, true)
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("plan = %v, want no error", err)
	}
	for _, e := range b.entries {
		if e.Action == ActionDelete {
			t.Fatalf("plan prunes %s %q the caller cannot see", e.Kind, e.Name)
		}
	}
}

func TestPlanPruneNamesOnlyVisibleDependents(t *testing.T) {
	rows, _, p := tenantRows("seen-bot", "hidden-bot")
	rows.Teams = nil
	b := hiddenPlanBuilder(rows, seesOnly{p.Meta.ID: true, rows.ServiceAccounts[0].Meta.ID: true}, true)
	err := b.run(context.Background(), nil)
	var de *project.DependentsError
	if !errors.As(err, &de) {
		t.Fatalf("plan = %v, want a dependents error for the visible project", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "seen-bot") || strings.Contains(msg, "hidden-bot") || !strings.Contains(msg, "1 more") {
		t.Fatalf("dependents error = %q, want seen-bot named and hidden-bot only counted", msg)
	}
}

func groupDoc(member string) manifest.Document {
	d := &manifest.GroupDTO{APIVersion: manifest.APIVersion, Kind: "Group"}
	d.Metadata.Name = "probe"
	d.Spec.Members = []string{member}
	return manifest.Document{Group: d}
}

// An unresolved name is typed so the API can answer it like a hidden row.
func TestPlanUnresolvedNameIsRefNotFound(t *testing.T) {
	rows := &Rows{Users: []*user.User{{ID: meta.NewID(), Username: "alice@example.test"}}}
	err := hiddenPlanBuilder(rows, seesOnly{}, false).run(context.Background(), []manifest.Document{groupDoc("bob@example.test")})
	var nf *manifest.RefNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("plan = %v, want a RefNotFoundError", err)
	}
	if err := hiddenPlanBuilder(rows, seesOnly{}, false).run(context.Background(), []manifest.Document{groupDoc("alice@example.test")}); err != nil {
		t.Fatalf("known user: plan = %v, want success up to authorization", err)
	}
}

func serviceAccountDoc(name, projectName string) manifest.Document {
	d := &manifest.ServiceAccountDTO{APIVersion: manifest.APIVersion, Kind: "ServiceAccount"}
	d.Metadata.Name = name
	d.Spec.Project = projectName
	return manifest.Document{ServiceAccount: d}
}

// Re-declaring a hidden row's name is refused before validation or the
// reference checks can describe the stored row.
func TestPlanRedeclaringHiddenRowIsForbidden(t *testing.T) {
	rows, _, theirs := tenantRows("shared-name")
	mine := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "mine"}, Spec: project.Spec{TeamID: meta.NewID()}}
	mine.StampOwner()
	rows.Projects = append(rows.Projects, mine)

	err := hiddenPlanBuilder(rows, seesOnly{mine.Meta.ID: true}, false).run(context.Background(), []manifest.Document{serviceAccountDoc("shared-name", "mine")})
	var ae *AuthzError
	if !errors.As(err, &ae) {
		t.Fatalf("plan = %v, want an AuthzError", err)
	}
	if strings.Contains(err.Error(), theirs.Meta.ID) {
		t.Fatalf("error names the hidden owner: %v", err)
	}

	// A caller who sees the row still gets the specific reason.
	seen := rows.ServiceAccounts[0].Meta.ID
	err = hiddenPlanBuilder(rows, seesOnly{mine.Meta.ID: true, seen: true}, false).run(context.Background(), []manifest.Document{serviceAccountDoc("shared-name", "mine")})
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("visible row: plan = %v, want an InvalidError", err)
	}
}
