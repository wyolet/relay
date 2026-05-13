package provider

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func TestValidate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		p := &Provider{
			Meta: meta.Metadata{Name: "openai"},
			Spec: Spec{BaseURL: "https://api.openai.com"},
		}
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
			p:    &Provider{Spec: Spec{BaseURL: "https://api.openai.com"}},
			want: "Name",
		},
		{
			name: "missing baseURL",
			p:    &Provider{Meta: meta.Metadata{Name: "x"}, Spec: Spec{}},
			want: "BaseURL",
		},
		{
			name: "bad URL",
			p:    &Provider{Meta: meta.Metadata{Name: "x"}, Spec: Spec{BaseURL: "not-a-url"}},
			want: "BaseURL",
		},
		{
			name: "non-system owner rejected",
			p: &Provider{
				Meta: meta.Metadata{Name: "x", Owner: meta.Owner{Kind: meta.OwnerUser}},
				Spec: Spec{BaseURL: "https://x.example"},
			},
			want: "owner.kind must be system",
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
