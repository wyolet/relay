package provider

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func TestValidate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		p := &Provider{Meta: meta.Metadata{Name: "anthropic"}}
		if err := p.Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		p    *Provider
		want string
	}{
		{
			name: "missing name",
			p:    &Provider{},
			want: "Name",
		},
		{
			name: "non-system owner rejected",
			p: &Provider{
				Meta: meta.Metadata{Name: "x", Owner: meta.Owner{Kind: meta.OwnerUser}},
			},
			want: "owner.kind must be system",
		},
		{
			name: "bad homepageURL",
			p: &Provider{
				Meta: meta.Metadata{Name: "x"},
				Spec: Spec{HomepageURL: "not-a-url"},
			},
			want: "HomepageURL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
		})
	}
}

// A provider is shared catalog data: no user, team or project owns one.
func TestValidate_TenantOwnersRefused(t *testing.T) {
	for _, o := range []meta.Owner{
		{Kind: meta.OwnerUser}, {Kind: meta.OwnerUser, ID: "u-1"},
		{Kind: meta.OwnerTeam, ID: "t-1"}, {Kind: meta.OwnerProject, ID: "p-1"},
	} {
		p := &Provider{Meta: meta.Metadata{Name: "acme", Owner: o}}
		if err := p.Validate(); err == nil {
			t.Errorf("owner %+v accepted", o)
		}
	}
}

func TestIsEnabled(t *testing.T) {
	tru, fls := true, false
	for _, tc := range []struct {
		name string
		val  *bool
		want bool
	}{
		{"nil defaults to true", nil, true},
		{"explicit true", &tru, true},
		{"explicit false", &fls, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{Spec: Spec{Enabled: tc.val}}
			if got := p.IsEnabled(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
