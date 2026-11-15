package team

import (
	"testing"

	"github.com/wyolet/relay/pkg/meta"
)

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner meta.OwnerKind
		want  string
	}{
		{"user owner", meta.OwnerUser, ""},
		{"system owner", meta.OwnerSystem, ""},
		{"provider owner", meta.OwnerProvider, `team "platform": owner.kind must be user or system, got "provider"`},
		{"team owner", meta.OwnerTeam, `team "platform": owner.kind must be user or system, got "team"`},
		{"no owner", "", `team "platform": owner.kind must be user or system, got ""`},
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

func TestIsEnabled(t *testing.T) {
	off, on := false, true
	for _, tc := range []struct {
		enabled *bool
		want    bool
	}{{nil, true}, {&on, true}, {&off, false}} {
		if got := (&Team{Spec: Spec{Enabled: tc.enabled}}).IsEnabled(); got != tc.want {
			t.Errorf("IsEnabled with %v = %v, want %v", tc.enabled, got, tc.want)
		}
	}
}
