package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/meta"
)

// A bundle declaring a project and its service account can bind their accounts in the same run: the names resolve to accounts the run plans, owned where relay keeps them.
func TestApplyBindsCallerAccountsOfRowsItDeclares(t *testing.T) {
	b := &builder{opts: Options{Stores: &Stores{}}, rows: &Rows{}, idx: newIndex(&Rows{}), admin: true}
	doc := `kind: Team
metadata: {name: ml}
spec: {}
---
` + docHeader + `kind: Project
metadata: {name: search}
spec: {team: ml}
---
` + docHeader + `kind: ServiceAccount
metadata: {name: indexer}
spec: {project: search}
---
` + docHeader + `kind: Policy
metadata: {name: search-default, owner: {kind: project, name: search}}
spec: {}
---
` + docHeader + `kind: PolicyBinding
metadata: {name: search-everyone, owner: {kind: project, name: search}}
spec: {policy: search-default, accounts: [project-search, sa-indexer]}
`
	if err := b.run(context.Background(), userDocs(t, docHeader+doc)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	var pb *policybinding.PolicyBinding
	for _, v := range b.declared {
		if x, ok := v.(*policybinding.PolicyBinding); ok {
			pb = x
		}
	}
	if pb == nil || len(pb.Spec.AccountIDs) != 2 {
		t.Fatalf("binding = %+v, want two accounts", pb)
	}
	project := meta.Owner{Kind: meta.OwnerProject, ID: b.idx.Projects["search"]}
	for i, want := range []account.Kind{account.KindProject, account.KindService} {
		a := b.callerAccounts[pb.Spec.AccountIDs[i]]
		if a == nil || a.DerivedKind() != want || a.Meta.Owner != project {
			t.Errorf("account %d = %+v, want the %s account in the project", i, a, want)
		}
	}
}

// A document creates provider accounts only, and edits a caller account without moving it.
func TestApplyAccountDocumentsLeaveCallerAccountsToRelay(t *testing.T) {
	rows := tenancyRows(false)
	team := account.ForTeam(rows.Teams[0].Meta.ID, rows.Teams[0].Meta.Name, "")
	team.Meta.ID = meta.NewID()
	rows.Accounts = []*account.Account{team}
	plan := func(doc string) (*builder, error) {
		b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows), admin: true}
		return b, b.run(context.Background(), userDocs(t, docHeader+doc))
	}

	if _, err := plan("kind: Account\nmetadata: {name: team-new, owner: {kind: team, name: platform}}\nspec: {}\n"); err == nil || !strings.Contains(err.Error(), "spec.host is required") {
		t.Fatalf("a document creating a caller account = %v, want a refusal", err)
	}
	b, err := plan("kind: Account\nmetadata: {name: " + team.Meta.Name + ", displayName: Platform, owner: {kind: system}}\nspec: {payloadLogging: on}\n")
	if err != nil {
		t.Fatalf("a document editing the team account: %v", err)
	}
	got, _ := b.declared[team.Meta.ID].(*account.Account)
	if got == nil || got.Meta.Owner != team.Meta.Owner || got.Spec.PayloadLogging != account.PayloadLoggingOn || got.Meta.DisplayName != "Platform" {
		t.Fatalf("edited team account = %+v, want its owner kept and the edit applied", got)
	}
}
