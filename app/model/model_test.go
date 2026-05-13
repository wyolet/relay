package model

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func validProvID() string { return meta.NewID() }

func TestValidate(t *testing.T) {
	t.Run("ok minimal", func(t *testing.T) {
		m := &Model{
			Meta: meta.Metadata{Name: "gpt-4o"},
			Spec: Spec{ProviderID: validProvID(), UpstreamName: "gpt-4o-2024-08-06"},
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		m    *Model
		want string
	}{
		{
			name: "missing name",
			m:    &Model{Spec: Spec{ProviderID: validProvID(), UpstreamName: "u"}},
			want: "Name",
		},
		{
			name: "missing providerID",
			m: &Model{
				Meta: meta.Metadata{Name: "x"},
				Spec: Spec{UpstreamName: "u"},
			},
			want: "ProviderID",
		},
		{
			name: "providerID not uuid",
			m: &Model{
				Meta: meta.Metadata{Name: "x"},
				Spec: Spec{ProviderID: "not-a-uuid", UpstreamName: "u"},
			},
			want: "ProviderID",
		},
		{
			name: "missing upstreamName",
			m: &Model{
				Meta: meta.Metadata{Name: "x"},
				Spec: Spec{ProviderID: validProvID()},
			},
			want: "UpstreamName",
		},
		{
			name: "user owner rejected",
			m: &Model{
				Meta: meta.Metadata{Name: "x", Owner: meta.Owner{Kind: meta.OwnerUser}},
				Spec: Spec{ProviderID: validProvID(), UpstreamName: "u"},
			},
			want: "owner.kind must be provider",
		},
		{
			name: "duplicate alias",
			m: &Model{
				Meta: meta.Metadata{Name: "x"},
				Spec: Spec{
					ProviderID:   validProvID(),
					UpstreamName: "u",
					Aliases:      []string{"GPT-4o", "gpt-4o"},
				},
			},
			want: "duplicate alias",
		},
		{
			name: "bad deprecation status",
			m: &Model{
				Meta: meta.Metadata{Name: "x"},
				Spec: Spec{
					ProviderID:   validProvID(),
					UpstreamName: "u",
					Deprecation:  &Deprecation{Status: "bogus"},
				},
			},
			want: "Status",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.m.Validate()
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
			m := &Model{Spec: Spec{Enabled: tc.val}}
			if got := m.IsEnabled(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
