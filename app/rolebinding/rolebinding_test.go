package rolebinding

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix(scope meta.Owner, subjects ...Subject) *RoleBinding {
	b := &RoleBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "platform-admins"},
		Spec: Spec{RoleID: meta.NewID(), Scope: scope, Subjects: subjects},
	}
	b.StampOwner()
	return b
}

func groupSubject(name string) Subject { return Subject{Kind: SubjectGroup, Name: name} }
func userSubject(id string) Subject    { return Subject{Kind: SubjectUser, ID: id} }

func TestValidate(t *testing.T) {
	teamID := meta.NewID()
	for _, tc := range []struct {
		name string
		b    *RoleBinding
		want string // "" = valid
	}{
		{
			name: "global scope, group subject",
			b:    fix(meta.Owner{Kind: meta.OwnerSystem}, groupSubject("platform-eng")),
		},
		{
			name: "team scope, user subject",
			b:    fix(meta.Owner{Kind: meta.OwnerTeam, ID: teamID}, userSubject(meta.NewID())),
		},
		{
			name: "global scope with an id",
			b:    fix(meta.Owner{Kind: meta.OwnerSystem, ID: teamID}, groupSubject("platform-eng")),
			want: "global scope carries no id",
		},
		{
			name: "team scope without an id",
			b:    fix(meta.Owner{Kind: meta.OwnerTeam}, groupSubject("platform-eng")),
			want: "requires an id",
		},
		{
			name: "user scope kind",
			b:    fix(meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}, groupSubject("platform-eng")),
			want: "scope.kind must be system, team, or project",
		},
		{
			name: "host scope kind",
			b:    fix(meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()}, groupSubject("platform-eng")),
			want: "scope.kind must be system, team, or project",
		},
		{
			name: "owner drifted from scope",
			b: func() *RoleBinding {
				b := fix(meta.Owner{Kind: meta.OwnerTeam, ID: teamID}, groupSubject("platform-eng"))
				b.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}
				return b
			}(),
			want: "does not mirror scope",
		},
		{
			name: "group subject carrying an id",
			b: fix(meta.Owner{Kind: meta.OwnerSystem},
				Subject{Kind: SubjectGroup, Name: "platform-eng", ID: meta.NewID()}),
			want: "group subject names a group",
		},
		{
			name: "user subject carrying a name",
			b: fix(meta.Owner{Kind: meta.OwnerSystem},
				Subject{Kind: SubjectUser, ID: meta.NewID(), Name: "alice"}),
			want: "carries an id, not a name",
		},
		{
			name: "service account subject without an id",
			b:    fix(meta.Owner{Kind: meta.OwnerSystem}, Subject{Kind: SubjectServiceAccount}),
			want: "carries an id, not a name",
		},
		{
			name: "duplicate subjects",
			b:    fix(meta.Owner{Kind: meta.OwnerSystem}, groupSubject("platform-eng"), groupSubject("platform-eng")),
			want: "duplicate subject",
		},
		{
			name: "no subjects",
			b:    fix(meta.Owner{Kind: meta.OwnerSystem}),
			want: "Subjects",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.b.Validate()
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

func TestSubjectKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		sub  Subject
		want string
	}{
		{name: "user", sub: Subject{Kind: SubjectUser, ID: "u-1"}, want: "user:u-1"},
		{name: "group", sub: Subject{Kind: SubjectGroup, Name: "platform-eng"}, want: "group:platform-eng"},
		{name: "service account", sub: Subject{Kind: SubjectServiceAccount, ID: "sa-1"}, want: "serviceaccount:sa-1"},
		{name: "virtual group", sub: Subject{Kind: SubjectGroup, Name: "system:authenticated"}, want: "group:system:authenticated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sub.Key(); got != tc.want {
				t.Fatalf("Key() = %q, want %q", got, tc.want)
			}
		})
	}
}
