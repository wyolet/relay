package policybinding

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/rolebinding"
)

func fix() *PolicyBinding {
	b := &PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search-everyone"},
		Spec: Spec{
			ProjectID: meta.NewID(),
			PolicyID:  meta.NewID(),
			Subjects: []rolebinding.Subject{
				{Kind: rolebinding.SubjectUser, ID: meta.NewID()},
			},
		},
	}
	b.StampOwner()
	return b
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    *PolicyBinding
		want string // "" = valid
	}{
		{
			name: "project owner mirrors projectId",
			b:    fix(),
		},
		{
			name: "team owner without a project",
			b: func() *PolicyBinding {
				b := fix()
				b.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}
				b.Spec.ProjectID = ""
				return b
			}(),
		},
		{
			name: "system owner without a project",
			b: func() *PolicyBinding {
				b := fix()
				b.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem}
				b.Spec.ProjectID = ""
				return b
			}(),
		},
		{
			name: "team owner with a projectId",
			b: func() *PolicyBinding {
				b := fix()
				b.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}
				return b
			}(),
			want: "spec.projectId must be empty",
		},
		{
			name: "user owner rejected",
			b: func() *PolicyBinding {
				b := fix()
				b.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()}
				b.Spec.ProjectID = ""
				return b
			}(),
			want: "owner.kind must be system, team or project",
		},
		{
			name: "owner id drifted from projectId",
			b: func() *PolicyBinding {
				b := fix()
				b.Meta.Owner.ID = meta.NewID()
				return b
			}(),
			want: "does not match spec.projectId",
		},
		{
			name: "every subject kind",
			b: func() *PolicyBinding {
				b := fix()
				b.Spec.Subjects = []rolebinding.Subject{
					{Kind: SubjectHost, ID: meta.NewID()},
					{Kind: SubjectAccount, ID: meta.NewID()},
					{Kind: SubjectCredential, ID: meta.NewID()},
					{Kind: SubjectTeam, ID: meta.NewID()},
					{Kind: SubjectProject, ID: meta.NewID()},
					{Kind: rolebinding.SubjectUser, ID: meta.NewID()},
					{Kind: rolebinding.SubjectServiceAccount, ID: meta.NewID()},
				}
				return b
			}(),
		},
		{
			name: "a relay key is never a subject",
			b: func() *PolicyBinding {
				b := fix()
				b.Spec.Subjects = []rolebinding.Subject{{Kind: "key", ID: meta.NewID()}}
				return b
			}(),
			want: `kind "key" is not one of`,
		},
		{
			name: "missing subject kind",
			b: func() *PolicyBinding {
				b := fix()
				b.Spec.Subjects = []rolebinding.Subject{{ID: meta.NewID()}}
				return b
			}(),
			want: "is not one of",
		},
		{
			name: "host subject id must be a UUID",
			b: func() *PolicyBinding {
				b := fix()
				b.Spec.Subjects = []rolebinding.Subject{{Kind: SubjectHost, ID: "openai"}}
				return b
			}(),
			want: "is not a UUID",
		},
		{
			name: "account subject carries an id, not a name",
			b: func() *PolicyBinding {
				b := fix()
				b.Spec.Subjects = []rolebinding.Subject{{Kind: SubjectAccount, Name: "acme-openai"}}
				return b
			}(),
			want: "account subject carries an id",
		},
		{
			name: "same credential twice",
			b: func() *PolicyBinding {
				b := fix()
				id := meta.NewID()
				b.Spec.Subjects = []rolebinding.Subject{{Kind: SubjectCredential, ID: id}, {Kind: SubjectCredential, ID: id}}
				return b
			}(),
			want: "duplicate subject",
		},
		{
			name: "a group is never a subject",
			b: func() *PolicyBinding {
				b := fix()
				b.Spec.Subjects = append(b.Spec.Subjects, rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"})
				return b
			}(),
			want: "groups are not policy subjects in this version",
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
