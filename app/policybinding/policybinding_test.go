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
				{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"},
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
			name: "explicit priority",
			b: func() *PolicyBinding {
				b := fix()
				b.Spec.Priority = 50
				return b
			}(),
		},
		{
			name: "priority above the ceiling",
			b: func() *PolicyBinding {
				b := fix()
				b.Spec.Priority = 10001
				return b
			}(),
			want: "Priority",
		},
		{
			name: "team owner rejected",
			b: func() *PolicyBinding {
				b := fix()
				b.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}
				return b
			}(),
			want: "owner.kind must be project",
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
			name: "group subject carrying an id",
			b: func() *PolicyBinding {
				b := fix()
				b.Spec.Subjects[0].ID = meta.NewID()
				return b
			}(),
			want: "group subject names a group",
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

func TestEffectivePriority(t *testing.T) {
	b := fix()
	if got := b.EffectivePriority(); got != DefaultPriority {
		t.Errorf("unset priority = %d, want %d", got, DefaultPriority)
	}
	b.Spec.Priority = 10
	if got := b.EffectivePriority(); got != 10 {
		t.Errorf("explicit priority = %d, want 10", got)
	}
}
