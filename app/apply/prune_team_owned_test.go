package apply

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	apppool "github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/team"
)

// teamOwnedRows is a pruned team without projects that owns one row of every kind whose owner lives only in metadata, plus a default account no key names; ownedLabelled decides whether the owned rows match the prune selector too.
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
	keyless := &account.Account{Meta: md("team-default"), Spec: account.Spec{HostID: hostID, Default: true}}
	keyless.Meta.Labels = nil
	return &Rows{
		Teams:          []*team.Team{tm},
		Policies:       []*policy.Policy{{Meta: md("team-policy")}},
		HostKeys:       []*hostkey.HostKey{{Meta: md("team-key"), Spec: hostkey.Spec{HostID: hostID, AccountID: named.Meta.ID}}},
		RateLimits:     []*ratelimit.RateLimit{{Meta: md("team-limit")}},
		Accounts:       []*account.Account{named, keyless},
		Pools:          []*apppool.Pool{{Meta: md("team-pool"), Spec: apppool.Spec{HostID: hostID}}},
		Budgets:        []*budget.Budget{{Meta: md("team-budget"), Spec: budget.Spec{AmountUSD: "10"}}},
		PolicyBindings: []*policybinding.PolicyBinding{{Meta: md("team-binding")}},
	}
}

// A team's prune is refused while it owns rows its delete would orphan or cascade to, as the control API's delete is; a default account no key names goes with the team.
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

// Rows the same run prunes do not block their team, and a keyless default account never does.
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
	if len(deleted) == 0 || deleted[len(deleted)-1] != "Team/platform" {
		t.Fatalf("deletes = %v, want the team last", deleted)
	}
	for _, d := range deleted {
		if d == "Account/team-default" {
			t.Fatalf("deletes = %v: the keyless default account goes with the team, not as its own delete", deleted)
		}
	}
}
