package account

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix() *Account {
	return &Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-openai", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}},
		Spec: Spec{HostID: meta.NewID(), OrgID: "org-abc123"},
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(a *Account)
		want string // "" = valid
	}{
		{name: "team owner with org id"},
		{name: "org id not known yet", edit: func(a *Account) { a.Spec.OrgID = "" }},
		{name: "system owner", edit: func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }},
		{name: "user owner", edit: func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()} }},
		{name: "project owner", edit: func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()} }},
		{name: "a caller account carries no org id", edit: func(a *Account) { a.Spec.HostID = "" }, want: "provider accounts only"},
		{name: "host must be an id", edit: func(a *Account) { a.Spec.HostID = "openai" }, want: "HostID"},
		{name: "team owner without id", edit: func(a *Account) { a.Meta.Owner.ID = "" }, want: "requires an id"},
		{name: "host owner rejected", edit: func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()} }, want: "owner.kind must be"},
		{name: "missing owner rejected", edit: func(a *Account) { a.Meta.Owner = meta.Owner{} }, want: "owner.kind must be"},
		{name: "a provider account has no holder", edit: func(a *Account) { a.Spec.Holder = &Holder{Kind: HolderUser, ID: meta.NewID()} }, want: "has no holder"},
		{name: "a system account needs a host", edit: func(a *Account) {
			a.Spec.HostID, a.Spec.OrgID, a.Meta.Owner = "", "", meta.Owner{Kind: meta.OwnerSystem}
		}, want: "hostId is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fix()
			if tc.edit != nil {
				tc.edit(a)
			}
			err := a.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.want != "" && err == nil:
				t.Fatalf("Validate() = nil, want error mentioning %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("Validate() = %v, want error mentioning %q", err, tc.want)
			}
		})
	}
}

// The default mark lives in its column as the owner, not in the spec document, so the unique index sees it.
func TestDefaultMarkIsTheOwnerColumn(t *testing.T) {
	a := fix()
	a.Spec.Default = true
	p, err := toUpsertParams(a)
	if err != nil {
		t.Fatal(err)
	}
	if want := "team:" + a.Meta.Owner.ID; !p.DefaultOwner.Valid || p.DefaultOwner.String != want {
		t.Errorf("default_owner = %+v, want %q", p.DefaultOwner, want)
	}
	if strings.Contains(string(p.Spec), "default") {
		t.Errorf("spec document %s carries the default mark", p.Spec)
	}
	a.Spec.Default = false
	if p, _ := toUpsertParams(a); p.DefaultOwner.Valid {
		t.Errorf("default_owner = %+v for a plain account, want NULL", p.DefaultOwner)
	}
	for owner, want := range map[meta.Owner]string{
		{}:                                 "system",
		{Kind: meta.OwnerSystem}:           "system",
		{Kind: meta.OwnerUser}:             "system",
		{Kind: meta.OwnerUser, ID: "u1"}:   "user:u1",
		{Kind: meta.OwnerProject, ID: "p"}: "project:p",
	} {
		if got := defaultOwner(owner); got != want {
			t.Errorf("defaultOwner(%+v) = %q, want %q", owner, got, want)
		}
	}
}

// The kind follows from the host, the holder and the owner; each caller kind has one place.
func TestKindsAndPlaces(t *testing.T) {
	team, proj, usr, sa := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	projectScope := meta.Owner{Kind: meta.OwnerProject, ID: proj}
	for _, tc := range []struct {
		a     *Account
		kind  Kind
		place string
	}{
		{ForTeam(team, "acme", ""), KindTeam, "team:" + team},
		{ForProject(proj, "ml", ""), KindProject, "project:" + proj},
		{Personal(usr, "Alice@Example.com"), KindPersonal, "personal:" + usr},
		{ForServiceAccount(sa, "ci", "", proj), KindService, "service:" + sa},
		{Member(usr, "alice", projectScope, "ml", ""), KindMember, "member:project:" + proj + ":" + usr},
		{fix(), KindProvider, ""},
	} {
		if err := tc.a.Validate(); err != nil {
			t.Errorf("%s: Validate() = %v", tc.kind, err)
		}
		if tc.a.Spec.Kind != tc.kind || tc.a.ManagedKey() != tc.place {
			t.Errorf("%s: kind %q, place %q; want %q", tc.kind, tc.a.Spec.Kind, tc.a.ManagedKey(), tc.place)
		}
	}
	if p := Personal(usr, "Alice@Example.com"); p.Meta.Name != "user-alice-example-com" {
		t.Errorf("personal account name = %q", p.Meta.Name)
	}
}
