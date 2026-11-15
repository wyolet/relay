package account

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/pkg/meta"
)

const (
	acctID = "01a19bc3-0000-7000-8000-000000000001"
	teamID = "01a19bc3-0000-7000-8000-000000000002"
	projID = "01a19bc3-0000-7000-8000-000000000003"
	userID = "01a19bc3-0000-7000-8000-000000000004"
	saID   = "01a19bc3-0000-7000-8000-000000000005"
	hostID = "01a19bc3-0000-7000-8000-000000000006"
)

var (
	team    = meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
	project = meta.Owner{Kind: meta.OwnerProject, ID: projID}
	user    = meta.Owner{Kind: meta.OwnerUser, ID: userID}
)

// fix is a team's provider account with its org id known.
func fix() *Account {
	return &Account{Meta: meta.Metadata{ID: acctID, Name: "acme-openai", Owner: team}, Spec: Spec{HostID: hostID, OrgID: "org-abc123"}}
}

func validateErr(a *Account) string {
	if err := a.Validate(); err != nil {
		return err.Error()
	}
	return ""
}

// A key lands only where relay re-checks the membership itself: the principal's personal or service account, a member account that follows role bindings, or a provider account the key's user owns.
func TestTakesKeysOf(t *testing.T) {
	alice, other := Holder{Kind: HolderUser, ID: userID}, Holder{Kind: HolderUser, ID: teamID}
	sa := Holder{Kind: HolderServiceAccount, ID: saID}
	follows := Member(userID, "alice", project, "ml", "")
	follows.FollowsRoleBindings = true
	owned := fix()
	owned.Meta.Owner = user
	// A service account whose id equals a user's holds none of that user's accounts.
	saNamedLikeUser := Holder{Kind: HolderServiceAccount, ID: userID}
	for _, tc := range []struct {
		name string
		a    *Account
		by   Holder
		want bool
	}{
		{"personal, its user", Personal(userID, "alice"), alice, true},
		{"personal, another user", Personal(userID, "alice"), other, false},
		{"personal, a service account of the same id", Personal(userID, "alice"), saNamedLikeUser, false},
		{"service, its service account", ForServiceAccount(saID, "ci", "", projID), sa, true},
		{"service, a user", ForServiceAccount(saID, "ci", "", projID), alice, false},
		{"member following role bindings, its user", follows, alice, true},
		{"member following role bindings, another user", follows, other, false},
		{"member opened on first use, its user", Member(userID, "alice", project, "ml", ""), alice, false},
		{"provider owned by the user", owned, alice, true},
		{"provider owned by the user, another user", owned, other, false},
		{"provider owned by the user, a service account of the same id", owned, saNamedLikeUser, false},
		{"provider owned by a team", fix(), alice, false},
		{"team account", ForTeam(teamID, "acme", ""), alice, false},
		{"project account", ForProject(projID, "ml", ""), sa, false},
	} {
		if got := tc.a.TakesKeysOf(tc.by); got != tc.want {
			t.Errorf("%s: TakesKeysOf = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The kind follows from the host, the holder and the owner; each caller kind has one place and a name that reads apart from the others.
func TestKindsAndPlaces(t *testing.T) {
	for _, tc := range []struct {
		a                 *Account
		kind              Kind
		place             string
		name, displayName string
	}{
		{ForTeam(teamID, "acme", ""), KindTeam, "team:" + teamID, "team-acme", "acme"},
		{ForTeam(teamID, "acme", "Acme Inc"), KindTeam, "team:" + teamID, "team-acme", "Acme Inc"},
		{ForProject(projID, "ml", ""), KindProject, "project:" + projID, "project-ml", "ml"},
		{Personal(userID, "Alice@Example.com"), KindPersonal, "personal:" + userID, "user-alice-example-com", "Alice@Example.com"},
		{ForServiceAccount(saID, "ci", "", projID), KindService, "service:" + saID, "sa-ci", "ci"},
		{Member(userID, "alice", project, "ml", ""), KindMember, "member:project:" + projID + ":" + userID, "alice-project-ml", "alice in ml"},
		{Member(userID, "alice", team, "acme", "Acme Inc"), KindMember, "member:team:" + teamID + ":" + userID, "alice-team-acme", "alice in Acme Inc"},
		{fix(), KindProvider, "", "acme-openai", ""},
	} {
		if got := validateErr(tc.a); got != "" {
			t.Errorf("%s: Validate() = %s", tc.name, got)
		}
		if tc.a.Spec.Kind != tc.kind || tc.a.ManagedKey() != tc.place || tc.a.Meta.Name != tc.name || tc.a.Meta.DisplayName != tc.displayName {
			t.Errorf("%s: kind %q place %q name %q display %q; want %q %q %q %q", tc.name,
				tc.a.Spec.Kind, tc.a.ManagedKey(), tc.a.Meta.Name, tc.a.Meta.DisplayName, tc.kind, tc.place, tc.name, tc.displayName)
		}
	}
	if got := MemberKey(project, userID); got != "member:project:"+projID+":"+userID {
		t.Errorf("MemberKey = %q", got)
	}
	// A name with nothing a slug keeps still names the account.
	if p := Personal(userID, "@@@"); p.Meta.Name != "user" {
		t.Errorf("personal account of a symbol-only username is named %q, want user", p.Meta.Name)
	}
}

func TestValidate(t *testing.T) {
	holder := &Holder{Kind: HolderUser, ID: userID}
	for _, tc := range []struct {
		name string
		edit func(a *Account)
		want string
	}{
		{"team owner with org id", func(*Account) {}, ""},
		{"org id not known yet", func(a *Account) { a.Spec.OrgID = "" }, ""},
		{"system owner", func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }, ""},
		{"organization owner", func(a *Account) { a.Meta.Owner = meta.Global }, ""},
		{"organization owner with an id", func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerOrganization, ID: userID} }, `account "acme-openai": an organization owner carries no id`},
		{"user owner", func(a *Account) { a.Meta.Owner = user }, ""},
		{"user owner with the id left to be stamped", func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerUser} }, ""},
		{"project owner", func(a *Account) { a.Meta.Owner = project }, ""},
		{"payload logging off", func(a *Account) { a.Spec.PayloadLogging = PayloadLoggingOff }, ""},
		{"a caller account carries no org id", func(a *Account) { a.Spec.HostID = "" }, `account "acme-openai": orgId is for provider accounts only`},
		{"host must be an id", func(a *Account) { a.Spec.HostID = "openai" }, "Key: 'Account.Spec.HostID' Error:Field validation for 'HostID' failed on the 'uuid' tag"},
		{"payload logging outside on and off", func(a *Account) { a.Spec.PayloadLogging = "sometimes" }, "Key: 'Account.Spec.PayloadLogging' Error:Field validation for 'PayloadLogging' failed on the 'oneof' tag"},
		{"team owner without id", func(a *Account) { a.Meta.Owner.ID = "" }, `account "acme-openai": owner.kind "team" requires an id`},
		{"project owner without id", func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerProject} }, `account "acme-openai": owner.kind "project" requires an id`},
		{"host owner", func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: hostID} }, `account "acme-openai": owner.kind must be organization, system, team, project or user, got "host"`},
		{"no owner", func(a *Account) { a.Meta.Owner = meta.Owner{} }, `account "acme-openai": owner.kind must be organization, system, team, project or user, got ""`},
		{"a provider account has no holder", func(a *Account) { a.Spec.Holder = holder }, `account "acme-openai": a provider account (spec.hostId set) has no holder, except a service account's share, owned by its project`},
		{"a holder that is no id", func(a *Account) { a.Spec.Holder = &Holder{Kind: HolderUser, ID: "alice"} }, "Key: 'Account.Spec.Holder.ID' Error:Field validation for 'ID' failed on the 'uuid' tag"},
		{"a holder of an unknown kind", func(a *Account) { a.Spec.Holder = &Holder{Kind: "group", ID: userID} }, "Key: 'Account.Spec.Holder.Kind' Error:Field validation for 'Kind' failed on the 'oneof' tag"},
		{"a system account needs a host", func(a *Account) {
			a.Spec.HostID, a.Spec.OrgID, a.Meta.Owner = "", "", meta.Owner{Kind: meta.OwnerSystem}
		}, `account "acme-openai": spec.hostId is required unless the account is a team's, a project's, a user's personal account, a member account or a service account's`},
		{"a user's account held by another user fits no kind", func(a *Account) {
			a.Spec.HostID, a.Spec.OrgID, a.Meta.Owner, a.Spec.Holder = "", "", meta.Owner{Kind: meta.OwnerUser, ID: teamID}, holder
		}, `account "acme-openai": spec.hostId is required unless the account is a team's, a project's, a user's personal account, a member account or a service account's`},
		{"a team's account held by a service account fits no kind", func(a *Account) {
			a.Spec.HostID, a.Spec.OrgID, a.Spec.Holder = "", "", &Holder{Kind: HolderServiceAccount, ID: saID}
		}, `account "acme-openai": spec.hostId is required unless the account is a team's, a project's, a user's personal account, a member account or a service account's`},
	} {
		a := fix()
		tc.edit(a)
		if got := validateErr(a); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
		if tc.want != "" && a.Spec.Kind != "" {
			t.Errorf("%s: a refused account was given the kind %q", tc.name, a.Spec.Kind)
		}
	}
}

// A share is a provider account owned by its receiver, a service account's by its project and held by it, and keyed by receiver so one account is shared with one receiver once.
func TestShare(t *testing.T) {
	src := fix()
	for _, tc := range []struct {
		name       string
		r          Receiver
		owner      meta.Owner
		wantHolder *Holder
	}{
		{"team", Receiver{Kind: ReceiverTeam, ID: teamID}, team, nil},
		{"user", Receiver{Kind: ReceiverUser, ID: userID}, user, nil},
		{"service account", Receiver{Kind: ReceiverServiceAccount, ID: saID}, project, &Holder{Kind: HolderServiceAccount, ID: saID}},
	} {
		a := Share(src, tc.r, tc.owner, "Platform")
		a.Meta.ID = projID
		if got := validateErr(a); got != "" {
			t.Fatalf("%s: Validate() = %s", tc.name, got)
		}
		want := &Account{
			Meta: meta.Metadata{ID: projID, Name: "acme-openai-for-platform", DisplayName: "acme-openai", Owner: tc.owner},
			Spec: Spec{Kind: KindProvider, Holder: tc.wantHolder, HostID: hostID, SourceAccountID: acctID},
		}
		if diff := cmp.Diff(want, a); diff != "" {
			t.Errorf("%s: share (-want +got):\n%s", tc.name, diff)
		}
		if !a.IsShared() || a.ManagedKey() != "" || a.Receiver() != tc.r || a.Receiver().Key() != string(tc.r.Kind)+":"+tc.r.ID {
			t.Errorf("%s: shared %v place %q receiver %+v, want a share with no place and receiver %+v", tc.name, a.IsShared(), a.ManagedKey(), a.Receiver(), tc.r)
		}
		p, err := toUpsertParams(a)
		if err != nil || p.SourceAccountID.String != acctID || p.ShareReceiver.String != tc.r.Key() || p.Kind != "provider" || p.ManagedKey.Valid {
			t.Errorf("%s: params source %+v receiver %+v kind %q place %+v err %v", tc.name, p.SourceAccountID, p.ShareReceiver, p.Kind, p.ManagedKey, err)
		}
	}
	named := fix()
	named.Meta.Name, named.Meta.DisplayName = "@@@", "Acme OpenAI"
	if a := Share(named, Receiver{Kind: ReceiverTeam, ID: teamID}, team, "@@@"); a.Meta.Name != "for" || a.Meta.DisplayName != "Acme OpenAI" {
		t.Errorf("share of a symbol-only name = %q display %q, want for and the source's display name", a.Meta.Name, a.Meta.DisplayName)
	}
	if fix().IsShared() || (&Holder{Kind: HolderUser, ID: "u"}).Key() != "user:u" {
		t.Error("a real account reads as shared, or a holder key is not kind:id")
	}
}

func TestValidateShare(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(a *Account)
		want string
	}{
		{"a team's share", func(*Account) {}, ""},
		{"org id stays on the real account", func(a *Account) { a.Spec.OrgID = "org" }, `account "acme-openai-for-t": a shared account takes its org and keys from the real account; orgId stays on that one`},
		{"the system receives no share", func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }, `account "acme-openai-for-t": a shared account is owned by the user, team or project it was shared with`},
		{"a user holder is no share's", func(a *Account) { a.Spec.Holder = &Holder{Kind: HolderUser, ID: userID} }, `account "acme-openai-for-t": a provider account (spec.hostId set) has no holder, except a service account's share, owned by its project`},
		{"a service account's share outside a project", func(a *Account) { a.Spec.Holder = &Holder{Kind: HolderServiceAccount, ID: saID} }, `account "acme-openai-for-t": a provider account (spec.hostId set) has no holder, except a service account's share, owned by its project`},
		{"not shared with itself", func(a *Account) { a.Spec.SourceAccountID = a.Meta.ID }, `account "acme-openai-for-t": an account cannot be shared with itself`},
		{"a source that is no id", func(a *Account) { a.Spec.SourceAccountID = "acme-openai" }, "Key: 'Account.Spec.SourceAccountID' Error:Field validation for 'SourceAccountID' failed on the 'uuid' tag"},
		{"a caller account is never a share", func(a *Account) { a.Spec.HostID = "" }, `account "acme-openai-for-t": only a provider account is shared`},
	} {
		a := Share(fix(), Receiver{Kind: ReceiverTeam, ID: teamID}, team, "t")
		a.Meta.ID = projID
		tc.edit(a)
		if got := validateErr(a); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}

// The grouping mark and the derived fields are server-managed: a write carrying them puts none in the stored spec document, so no caller can mark an account grouped by relay.
func TestStoredSpecKeepsOnlyTheFlagsWithoutAColumn(t *testing.T) {
	a := fix()
	a.Spec.GroupedByRelay, a.Spec.Closed, a.Spec.PayloadLogging, a.Spec.SharedBy = true, true, PayloadLoggingOn, &ShareParty{Kind: "team"}
	p, err := toUpsertParams(a)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"kind":"","orgId":"org-abc123","payloadLogging":"on"}`; string(p.Spec) != want {
		t.Errorf("spec document = %s, want %s", p.Spec, want)
	}
	nowhere := &Account{Meta: meta.Metadata{Name: "x", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if _, err := toUpsertParams(nowhere); err == nil || err.Error() != `account "x" fits no kind` {
		t.Errorf("an account that fits no kind: err = %v", err)
	}
}
