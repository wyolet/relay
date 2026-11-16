package team

import (
	"testing"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/pkg/meta"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner meta.OwnerKind
		want  string
	}{
		{"user owner", meta.OwnerUser, ""},
		{"organization owner", meta.OwnerOrganization, ""},
		{"system owner", meta.OwnerSystem, ""},
		{"provider owner", meta.OwnerProvider, `team "platform": owner.kind must be user or organization, got "provider"`},
		{"team owner", meta.OwnerTeam, `team "platform": owner.kind must be user or organization, got "team"`},
		{"no owner", "", `team "platform": owner.kind must be user or organization, got ""`},
	} {
		tm := &Team{Meta: meta.Metadata{ID: "01a19bc3-0000-7000-8000-000000000001", Name: "platform", Owner: meta.Owner{Kind: tc.owner}}}
		got := ""
		if err := tm.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}

// A team stored as system-owned, before the global scope was named organization, reads as the organization's.
func TestFromDirectoryReadsSystemAsTheOrganization(t *testing.T) {
	for stored, want := range map[string]meta.Owner{
		directory.OwnerSystem: meta.Global,
		"organization":        meta.Global,
		directory.OwnerUser:   {Kind: meta.OwnerUser, ID: "u-1"},
	} {
		row := &directory.Team{Meta: directory.Metadata{ID: "t-1", Name: "platform"}}
		row.Meta.Owner.Kind = stored
		if stored == directory.OwnerUser {
			row.Meta.Owner.ID = "u-1"
		}
		got, err := FromDirectory(row)
		if err != nil || got.Meta.Owner != want {
			t.Errorf("stored %q: owner %+v (%v), want %+v", stored, got.Meta.Owner, err, want)
		}
	}
}
