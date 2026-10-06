package host

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix(name string) *Host {
	return &Host{
		Meta: meta.Metadata{Name: name, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: Spec{BaseURL: "https://api.example.com"},
	}
}

func TestValidate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		if err := fix("openai-direct").Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	t.Run("ok empty baseURL", func(t *testing.T) {
		h := fix("ollama-self")
		h.Spec.BaseURL = ""
		if err := h.Validate(); err != nil {
			t.Fatalf("empty baseURL should be accepted: %v", err)
		}
		if h.Routable() {
			t.Fatal("a host with no baseURL must not be routable")
		}
	})

	t.Run("ok empty owner", func(t *testing.T) {
		h := fix("openai-direct")
		h.Meta.Owner = meta.Owner{}
		if err := h.Validate(); err != nil {
			t.Fatalf("empty owner should be accepted: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		h    *Host
		want string
	}{
		{
			name: "missing name",
			h:    func() *Host { h := fix("x"); h.Meta.Name = ""; return h }(),
			want: "Name",
		},
		{
			name: "bad baseURL",
			h:    func() *Host { h := fix("x"); h.Spec.BaseURL = "not-a-url"; return h }(),
			want: "BaseURL",
		},
		{
			name: "provider owner rejected",
			h:    func() *Host { h := fix("x"); h.Meta.Owner.Kind = meta.OwnerProvider; return h }(),
			want: "owner.kind must be system or omitted",
		},
		{
			name: "user owner rejected",
			h:    func() *Host { h := fix("x"); h.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}; return h }(),
			want: "hosts are shared catalog data",
		},
		{
			name: "id-less user owner rejected",
			h:    func() *Host { h := fix("x"); h.Meta.Owner = meta.Owner{Kind: meta.OwnerUser}; return h }(),
			want: "hosts are shared catalog data",
		},
		{
			name: "team owner rejected",
			h:    func() *Host { h := fix("x"); h.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: "t-1"}; return h }(),
			want: "hosts are shared catalog data",
		},
		{
			name: "project owner rejected",
			h:    func() *Host { h := fix("x"); h.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: "p-1"}; return h }(),
			want: "hosts are shared catalog data",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.h.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestHostValidate_DeploymentOwnersAllowed(t *testing.T) {
	for _, kind := range []meta.OwnerKind{"", meta.OwnerSystem} {
		h := fix("x")
		h.Meta.Owner = meta.Owner{Kind: kind}
		if err := h.Validate(); err != nil {
			t.Fatalf("owner %q: %v", kind, err)
		}
	}
}
