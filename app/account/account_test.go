package account

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/pkg/meta"
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

// The grouping mark is server-managed: a write carrying it puts it nowhere, so no caller can mark an account grouped by relay.
func TestGroupedByRelayIsNeverWritten(t *testing.T) {
	a := fix()
	a.Spec.GroupedByRelay = true
	p, err := toUpsertParams(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.Spec), "grouped") {
		t.Errorf("spec document %s carries the grouping mark", p.Spec)
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

// A share is a provider account owned by its receiver, a service account's by its project and held by it, and keyed by receiver so one account is shared with one receiver once.
func TestShare(t *testing.T) {
	src := fix()
	proj, sa, team := meta.NewID(), meta.NewID(), meta.NewID()
	for _, tc := range []struct {
		name  string
		r     Receiver
		owner meta.Owner
		key   string
	}{
		{"team", Receiver{Kind: ReceiverTeam, ID: team}, meta.Owner{Kind: meta.OwnerTeam, ID: team}, "team:" + team},
		{"service account", Receiver{Kind: ReceiverServiceAccount, ID: sa}, meta.Owner{Kind: meta.OwnerProject, ID: proj}, "serviceaccount:" + sa},
	} {
		a := Share(src, tc.r, tc.owner, "Platform")
		a.Meta.ID = meta.NewID()
		if err := a.Validate(); err != nil {
			t.Fatalf("%s: Validate() = %v", tc.name, err)
		}
		if a.Spec.Kind != KindProvider || !a.IsShared() || a.Spec.HostID != src.Spec.HostID || a.ManagedKey() != "" {
			t.Errorf("%s: kind %q shared %v host %q place %q", tc.name, a.Spec.Kind, a.IsShared(), a.Spec.HostID, a.ManagedKey())
		}
		if a.Meta.Name != "acme-openai-for-platform" || a.Receiver().Key() != tc.key {
			t.Errorf("%s: name %q receiver %q, want acme-openai-for-platform %q", tc.name, a.Meta.Name, a.Receiver().Key(), tc.key)
		}
		p, err := toUpsertParams(a)
		if err != nil || p.SourceAccountID.String != src.Meta.ID || p.ShareReceiver.String != tc.key {
			t.Errorf("%s: params source %+v receiver %+v err %v", tc.name, p.SourceAccountID, p.ShareReceiver, err)
		}
	}
	if a := fix(); a.TakesKeysOf(Holder{Kind: HolderServiceAccount, ID: sa}) {
		t.Error("a service account's key lands in a provider account")
	}
}

func TestValidateShare(t *testing.T) {
	share := func() *Account {
		a := Share(fix(), Receiver{Kind: ReceiverTeam, ID: "t"}, meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}, "t")
		a.Meta.ID = meta.NewID()
		return a
	}
	for _, tc := range []struct {
		name string
		edit func(a *Account)
		want string
	}{
		{name: "org id stays on the real account", edit: func(a *Account) { a.Spec.OrgID = "org" }, want: "orgId stays"},
		{name: "the system receives no share", edit: func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }, want: "owned by the user, team or project"},
		{name: "a user holder is no share's", edit: func(a *Account) { a.Spec.Holder = &Holder{Kind: HolderUser, ID: meta.NewID()} }, want: "has no holder"},
		{name: "a service account's share outside a project", edit: func(a *Account) { a.Spec.Holder = &Holder{Kind: HolderServiceAccount, ID: meta.NewID()} }, want: "has no holder"},
		{name: "not shared with itself", edit: func(a *Account) { a.Spec.SourceAccountID = a.Meta.ID }, want: "with itself"},
	} {
		a := share()
		tc.edit(a)
		if err := a.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want error mentioning %q", tc.name, err, tc.want)
		}
	}
}
