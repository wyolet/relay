package manifest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/auth/authz"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/manifest"
)

const teamOwnedBundle = `apiVersion: auth.wyolet.dev/v1alpha1
kind: Team
metadata: {name: platform, labels: {managed-by: gitops}}
---
apiVersion: auth.wyolet.dev/v1alpha1
kind: ServiceAccount
metadata: {name: deployer, labels: {managed-by: gitops}}
spec: {team: platform}
`

// A ServiceAccount document naming spec.team is owned by that team, with no project, and renders back to the same document.
func TestTeamOwnedServiceAccountRoundTrips(t *testing.T) {
	e := newEnv(t)
	e.apply(t, teamOwnedBundle, manifest.Options{})
	ctx := context.Background()
	s := e.store.Stores()
	teams, _ := s.Teams.List(ctx)
	sas, _ := s.ServiceAccounts.List(ctx)
	if len(teams) != 1 || len(sas) != 1 {
		t.Fatalf("stored %d teams and %d service accounts, want one each", len(teams), len(sas))
	}
	if diff := cmp.Diff(authz.Owner{Kind: directory.OwnerTeam, ID: teams[0].Meta.ID}, sas[0].Meta.Owner); diff != "" {
		t.Errorf("owner (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(directory.ServiceAccountSpec{TeamID: teams[0].Meta.ID}, sas[0].Spec); diff != "" {
		t.Errorf("spec (-want +got):\n%s", diff)
	}

	names := storeNames(t, e)
	team, err := e.reg.FromTeam(teams[0], names)
	must(t, err)
	sa, err := e.reg.FromServiceAccount(sas[0], names)
	must(t, err)
	if diff := cmp.Diff(&manifest.ServiceAccountSpec{Team: "platform"}, sa.ServiceAccount); diff != "" {
		t.Errorf("rendered spec (-want +got):\n%s", diff)
	}
	rendered, err := manifest.Render([]*manifest.Document{team, sa})
	must(t, err)
	for _, entry := range e.plan(t, e.parse(t, string(rendered)), manifest.Options{}).Entries {
		if entry.Action != manifest.ActionUnchanged {
			t.Errorf("%s %s/%s %v after a round trip, want unchanged", entry.Action, entry.Kind, entry.Name, entry.ChangedFields)
		}
	}
}

// A ServiceAccount document names exactly one owner.
func TestServiceAccountDocumentNamesOneOwner(t *testing.T) {
	e := newEnv(t)
	e.apply(t, bundle, manifest.Options{})
	for name, spec := range map[string]string{
		"neither":  "{}",
		"both":     "{project: search, team: platform}",
		"no team":  "{team: nowhere}",
		"no owner": "{enabled: true}",
	} {
		doc := "apiVersion: auth.wyolet.dev/v1alpha1\nkind: ServiceAccount\nmetadata: {name: bot}\nspec: " + spec + "\n"
		_, err := e.reg.ToServiceAccount(&e.parse(t, doc)[0], e.opts.Refs)
		if err == nil {
			t.Errorf("%s: translated, want an error", name)
			continue
		}
		want := "spec"
		if name == "no team" {
			want = "spec.team"
		}
		if got := asError(t, err); got.Field != want {
			t.Errorf("%s: error on %q (%v), want %q", name, got.Field, err, want)
		}
	}
}

// Pruning a team that still owns a stored service account the selector leaves out is refused while planning, before any write; selected too, both go, the service account first.
func TestPlanRefusesPruningATeamThatOwnsAKeptServiceAccount(t *testing.T) {
	prune := func(e *env) manifest.Options {
		return manifest.Options{Prune: true, Selector: map[string]string{"managed-by": "gitops"}, Refs: e.opts.Refs, Names: e.opts.Names}
	}
	e := newEnv(t)
	e.apply(t, strings.Replace(teamOwnedBundle, "{name: deployer, labels: {managed-by: gitops}}", "{name: deployer}", 1), manifest.Options{})
	if _, err := e.reg.Plan(context.Background(), e.store.Stores(), nil, prune(e)); !errors.Is(err, manifest.ErrTeamOwnsServiceAccount) {
		t.Fatalf("plan err = %v, want ErrTeamOwnsServiceAccount", err)
	}
	if teams, _ := e.store.Stores().Teams.List(context.Background()); len(teams) != 1 {
		t.Fatalf("teams after the refused plan = %d, want the team kept", len(teams))
	}

	e = newEnv(t)
	e.apply(t, teamOwnedBundle, manifest.Options{})
	p := e.apply(t, "", prune(e))
	if got := strings.Join(actions(p), ","); got != "delete ServiceAccount/deployer,delete Team/platform" {
		t.Fatalf("prune of both = %s", got)
	}
}

// Pruning a team refuses a bundle that keeps or adds a service account the team owns: the team cannot be deleted under it.
func TestPlanRefusesAServiceAccountWhoseTeamIsPruned(t *testing.T) {
	e := newEnv(t)
	e.apply(t, teamOwnedBundle, manifest.Options{})
	prune := manifest.Options{Prune: true, Selector: map[string]string{"managed-by": "gitops"}, Refs: e.opts.Refs, Names: e.opts.Names}
	kept := strings.Split(teamOwnedBundle, "---\n")[1]
	_, err := e.reg.Plan(context.Background(), e.store.Stores(), e.parse(t, kept), prune)
	if !errors.Is(err, manifest.ErrScopePruned) {
		t.Fatalf("plan err = %v, want ErrScopePruned", err)
	}
	if got := asError(t, err); got.Kind != manifest.KindServiceAccount || got.Name != "deployer" || got.Field != "spec.team" {
		t.Fatalf("error on %s %q %s, want the account's team", got.Kind, got.Name, got.Field)
	}
}
