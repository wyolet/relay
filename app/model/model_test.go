package model

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func validProvID() string { return meta.NewID() }

// fix builds a minimally-valid Model. Tests override fields they want to break.
func fix(name string) *Model {
	return &Model{
		Meta: meta.Metadata{
			Name:  name,
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: validProvID()},
		},
		Spec: Spec{
			Hosts: []HostBinding{{HostID: meta.NewID(), UpstreamName: "u"}},
		},
	}
}

func TestValidate(t *testing.T) {
	t.Run("ok minimal", func(t *testing.T) {
		if err := fix("gpt-4o").Validate(); err != nil {
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
			m:    func() *Model { m := fix("x"); m.Meta.Name = ""; return m }(),
			want: "Name",
		},
		{
			name: "missing hosts",
			m:    func() *Model { m := fix("x"); m.Spec.Hosts = nil; return m }(),
			want: "Hosts",
		},
		{
			name: "host binding missing upstreamName",
			m: func() *Model {
				m := fix("x")
				m.Spec.Hosts[0].UpstreamName = ""
				return m
			}(),
			want: "UpstreamName",
		},
		{
			name: "host binding missing hostId",
			m: func() *Model {
				m := fix("x")
				m.Spec.Hosts[0].HostID = ""
				return m
			}(),
			want: "HostID",
		},
		{
			name: "duplicate host binding",
			m: func() *Model {
				m := fix("x")
				dup := m.Spec.Hosts[0].HostID
				m.Spec.Hosts = append(m.Spec.Hosts, HostBinding{HostID: dup, UpstreamName: "u2"})
				return m
			}(),
			want: "duplicate host binding",
		},
		{
			name: "user owner rejected",
			m:    func() *Model { m := fix("x"); m.Meta.Owner.Kind = meta.OwnerUser; return m }(),
			want: "owner.kind must be provider",
		},
		{
			name: "system owner rejected",
			m:    func() *Model { m := fix("x"); m.Meta.Owner.Kind = meta.OwnerSystem; return m }(),
			want: "owner.kind must be provider",
		},
		{
			name: "owner id required",
			m:    func() *Model { m := fix("x"); m.Meta.Owner.ID = ""; return m }(),
			want: "owner.id is required",
		},
		{
			name: "duplicate alias",
			m: func() *Model {
				m := fix("x")
				m.Spec.Aliases = []string{"GPT-4o", "gpt-4o"}
				return m
			}(),
			want: "duplicate alias",
		},
		{
			name: "alias collides with own name",
			m: func() *Model {
				m := fix("gpt-4o")
				m.Spec.Aliases = []string{"GPT-4O"}
				return m
			}(),
			want: "collides with the model's own name",
		},
		{
			name: "bad deprecation status",
			m: func() *Model {
				m := fix("x")
				m.Spec.Deprecation = &Deprecation{Status: "bogus"}
				return m
			}(),
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
