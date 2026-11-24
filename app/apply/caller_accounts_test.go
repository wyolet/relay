package apply

import (
	"context"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/catalogkind/host"
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

// A service account a declared team owns gets its account planned in the team, so a binding in the same bundle can name it.
func TestApplyBindsTheAccountOfATeamServiceAccount(t *testing.T) {
	b := &builder{opts: Options{Stores: &Stores{}}, rows: &Rows{}, idx: newIndex(&Rows{}), admin: true}
	doc := `kind: Team
metadata: {name: ml}
spec: {}
---
` + docHeader + `kind: ServiceAccount
metadata: {name: deployer}
spec: {team: ml}
---
` + docHeader + `kind: Policy
metadata: {name: ml-default, owner: {kind: team, name: ml}}
spec: {}
---
` + docHeader + `kind: PolicyBinding
metadata: {name: ml-deployer, owner: {kind: team, name: ml}}
spec: {policy: ml-default, accounts: [sa-deployer]}
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
	if pb == nil || len(pb.Spec.AccountIDs) != 1 {
		t.Fatalf("binding = %+v, want one account", pb)
	}
	team := meta.Owner{Kind: meta.OwnerTeam, ID: b.idx.Teams["ml"]}
	if a := b.callerAccounts[pb.Spec.AccountIDs[0]]; a == nil || a.DerivedKind() != account.KindService || a.Meta.Owner != team {
		t.Errorf("account = %+v, want the service account's account in the team", a)
	}
	sa := entryFor(t, b, "ServiceAccount", "deployer")
	if sa.Action != ActionCreate || sa.owner != team {
		t.Errorf("service account entry = %+v, want a create owned by the team", sa)
	}
}

// A bundle declaring a user and their service account writes the user first, and plans the service account's account owned by the user, so a personal binding in the same bundle can name it.
func TestApplyBindsTheAccountOfAUsersServiceAccount(t *testing.T) {
	b := &builder{opts: Options{Stores: &Stores{}}, rows: &Rows{}, idx: newIndex(&Rows{}), admin: true}
	doc := userYAML("alice", "", "false") + "---\n" + docHeader + `kind: ServiceAccount
metadata: {name: alice-bot}
spec: {user: alice}
---
` + docHeader + `kind: Policy
metadata: {name: alice-default, owner: {kind: user, name: alice}}
spec: {}
---
` + docHeader + `kind: PolicyBinding
metadata: {name: alice-bot-binding, owner: {kind: user, name: alice}}
spec: {policy: alice-default, accounts: [sa-alice-bot]}
`
	if err := b.run(context.Background(), userDocs(t, doc)); err != nil {
		t.Fatalf("plan: %v", err)
	}
	var order []string
	for _, e := range b.entries {
		if e.Kind == "User" || e.Kind == "ServiceAccount" {
			order = append(order, e.Kind)
		}
	}
	if strings.Join(order, ",") != "User,ServiceAccount" {
		t.Errorf("write order = %v, want the user before their service account", order)
	}
	var pb *policybinding.PolicyBinding
	for _, v := range b.declared {
		if x, ok := v.(*policybinding.PolicyBinding); ok {
			pb = x
		}
	}
	if pb == nil || len(pb.Spec.AccountIDs) != 1 {
		t.Fatalf("binding = %+v, want one account", pb)
	}
	alice := meta.Owner{Kind: meta.OwnerUser, ID: b.idx.Users["alice"]}
	if a := b.callerAccounts[pb.Spec.AccountIDs[0]]; a == nil || a.DerivedKind() != account.KindService || a.Meta.Owner != alice {
		t.Errorf("account = %+v, want the service account's account owned by alice", a)
	}
	if sa := entryFor(t, b, "ServiceAccount", "alice-bot"); sa.owner != alice {
		t.Errorf("service account entry owner = %+v, want alice", sa.owner)
	}
}

// An Account document cannot make an account a user owns and their service account holds: without a host it is a caller account, which relay alone makes, and with one but no sourceAccount it is no share, which no holder fits.
func TestApplyAccountDocumentMakesNoUserServiceAccountAccount(t *testing.T) {
	for name, tc := range map[string]struct{ spec, want string }{
		"no host":   {"{serviceAccount: alice-bot}", "spec.host is required"},
		"no source": {"{host: acme-api, serviceAccount: alice-bot}", "has no holder, except a service account's share"},
	} {
		rows := tenancyRows(false)
		alice := &user.User{ID: meta.NewID(), Username: "alice"}
		bot := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: "alice-bot", Owner: meta.Owner{Kind: meta.OwnerUser, ID: alice.ID}}}
		bot.StampOwner()
		rows.Users, rows.ServiceAccounts = []*user.User{alice}, append(rows.ServiceAccounts, bot)
		rows.Hosts = []*host.Host{{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-api", Owner: meta.Owner{Kind: meta.OwnerSystem}}}}
		b := &builder{opts: Options{Stores: &Stores{}}, rows: rows, idx: newIndex(rows), admin: true}
		doc := docHeader + "kind: Account\nmetadata: {name: alice-bot-acct, owner: {kind: user, name: alice}}\nspec: " + tc.spec + "\n"
		if err := b.run(context.Background(), userDocs(t, doc)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want a refusal containing %q", name, err, tc.want)
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
