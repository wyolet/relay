package rolebinding

import (
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

const (
	teamID = "01a19bc3-0000-7000-8000-000000000001"
	userID = "01a19bc3-0000-7000-8000-000000000002"
)

func fix(scope meta.Owner, subjects ...Subject) *RoleBinding {
	b := &RoleBinding{
		Meta: meta.Metadata{ID: userID, Name: "platform-admins"},
		Spec: Spec{RoleID: teamID, Scope: scope, Subjects: subjects},
	}
	b.StampOwner()
	return b
}

func TestSubjectKey(t *testing.T) {
	for _, tc := range []struct {
		sub  Subject
		want string
	}{
		{Subject{Kind: SubjectUser, ID: "u-1"}, "user:u-1"},
		{Subject{Kind: SubjectGroup, Name: "platform-eng"}, "group:platform-eng"},
		{Subject{Kind: SubjectServiceAccount, ID: "sa-1"}, "serviceaccount:sa-1"},
		{Subject{Kind: SubjectGroup, Name: "system:authenticated"}, "group:system:authenticated"},
		// A group is keyed by its name and the others by id, whatever else the subject carries.
		{Subject{Kind: SubjectGroup, Name: "eng", ID: "ignored"}, "group:eng"},
		{Subject{Kind: SubjectUser, ID: "u-1", Name: "ignored"}, "user:u-1"},
	} {
		if got := tc.sub.Key(); got != tc.want {
			t.Errorf("%+v: Key() = %q, want %q", tc.sub, got, tc.want)
		}
	}
}

func TestIsEnabled(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		enabled *bool
		want    bool
	}{{nil, true}, {&on, true}, {&off, false}} {
		if got := (&RoleBinding{Spec: Spec{Enabled: tc.enabled}}).IsEnabled(); got != tc.want {
			t.Errorf("IsEnabled with %v = %v, want %v", tc.enabled, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	system := meta.Owner{Kind: meta.OwnerSystem}
	team := meta.Owner{Kind: meta.OwnerTeam, ID: teamID}
	group := Subject{Kind: SubjectGroup, Name: "platform-eng"}
	user := Subject{Kind: SubjectUser, ID: userID}
	edited := func(edit func(*RoleBinding)) *RoleBinding {
		b := fix(team, group)
		edit(b)
		return b
	}
	for _, tc := range []struct {
		name string
		b    *RoleBinding
		want string
	}{
		{"global scope, group subject", fix(system, group), ""},
		{"team scope, user subject", fix(team, user), ""},
		{"project scope, mixed subjects", fix(meta.Owner{Kind: meta.OwnerProject, ID: teamID}, user, group, Subject{Kind: SubjectServiceAccount, ID: teamID}), ""},
		{"a user and a service account of one id", fix(system, user, Subject{Kind: SubjectServiceAccount, ID: userID}), ""},
		{"global scope with an id", fix(meta.Owner{Kind: meta.OwnerSystem, ID: teamID}, group), `rolebinding "platform-admins": global scope carries no id`},
		{"team scope without an id", fix(meta.Owner{Kind: meta.OwnerTeam}, group), `rolebinding "platform-admins": scope.kind "team" requires an id`},
		{"project scope without an id", fix(meta.Owner{Kind: meta.OwnerProject}, group), `rolebinding "platform-admins": scope.kind "project" requires an id`},
		{"user scope kind", fix(meta.Owner{Kind: meta.OwnerUser, ID: userID}, group), `rolebinding "platform-admins": scope.kind must be system, team, or project, got "user"`},
		{"host scope kind", fix(meta.Owner{Kind: meta.OwnerHost, ID: userID}, group), `rolebinding "platform-admins": scope.kind must be system, team, or project, got "host"`},
		{"no scope", fix(meta.Owner{}, group), "Key: 'RoleBinding.Spec.Scope' Error:Field validation for 'Scope' failed on the 'required' tag"},
		{"owner id drifted from scope", edited(func(b *RoleBinding) { b.Meta.Owner.ID = userID }), `rolebinding "platform-admins": owner team/01a19bc3-0000-7000-8000-000000000002 does not mirror scope team/01a19bc3-0000-7000-8000-000000000001`},
		{"owner kind drifted from scope", edited(func(b *RoleBinding) { b.Meta.Owner.Kind = meta.OwnerProject }), `rolebinding "platform-admins": owner project/01a19bc3-0000-7000-8000-000000000001 does not mirror scope team/01a19bc3-0000-7000-8000-000000000001`},
		{"role id not an id", edited(func(b *RoleBinding) { b.Spec.RoleID = "admin" }), "Key: 'RoleBinding.Spec.RoleID' Error:Field validation for 'RoleID' failed on the 'uuid' tag"},
		{"no role id", edited(func(b *RoleBinding) { b.Spec.RoleID = "" }), "Key: 'RoleBinding.Spec.RoleID' Error:Field validation for 'RoleID' failed on the 'required' tag"},
		{"group subject carrying an id", fix(system, Subject{Kind: SubjectGroup, Name: "platform-eng", ID: userID}), `"platform-admins": group subject names a group, it carries no id`},
		{"group subject without a name", fix(system, Subject{Kind: SubjectGroup}), `"platform-admins": group subject names a group, it carries no id`},
		{"user subject carrying a name", fix(system, Subject{Kind: SubjectUser, ID: userID, Name: "alice"}), `"platform-admins": user subject carries an id, not a name`},
		{"user subject whose id is no uuid", fix(system, Subject{Kind: SubjectUser, ID: "alice"}), "Key: 'RoleBinding.Spec.Subjects[0].ID' Error:Field validation for 'ID' failed on the 'uuid' tag"},
		{"service account subject without an id", fix(system, Subject{Kind: SubjectServiceAccount}), `"platform-admins": serviceaccount subject carries an id, not a name`},
		{"unknown subject kind", fix(system, Subject{Kind: "team", ID: teamID}), "Key: 'RoleBinding.Spec.Subjects[0].Kind' Error:Field validation for 'Kind' failed on the 'oneof' tag"},
		{"duplicate group", fix(system, group, user, group), `"platform-admins": duplicate subject "group:platform-eng"`},
		{"duplicate user", fix(system, user, user), `"platform-admins": duplicate subject "user:01a19bc3-0000-7000-8000-000000000002"`},
		{"no subjects", fix(system), "Key: 'RoleBinding.Spec.Subjects' Error:Field validation for 'Subjects' failed on the 'required' tag"},
	} {
		got := ""
		if err := tc.b.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}
