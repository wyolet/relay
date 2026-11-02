package policy

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix(name string) *Policy {
	return &Policy{
		Meta: meta.Metadata{
			Name:  name,
			Owner: meta.Owner{Kind: meta.OwnerUser},
		},
		Spec: Spec{},
	}
}

func TestValidate(t *testing.T) {
	t.Run("ok minimal user-owned", func(t *testing.T) {
		if err := fix("p").Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})
	t.Run("ok system-owned", func(t *testing.T) {
		p := fix("p")
		p.Meta.Owner.Kind = meta.OwnerSystem
		if err := p.Validate(); err != nil {
			t.Fatalf("unexpected: %v", err)
		}
	})

	for _, tc := range []struct {
		name string
		p    *Policy
		want string
	}{
		{
			name: "missing name",
			p:    func() *Policy { p := fix("x"); p.Meta.Name = ""; return p }(),
			want: "Name",
		},
		{
			name: "provider owner rejected",
			p: func() *Policy {
				p := fix("x")
				p.Meta.Owner = meta.Owner{Kind: meta.OwnerProvider, ID: meta.NewID()}
				return p
			}(),
			want: "owner.kind required",
		},
		{
			name: "missing owner",
			p:    func() *Policy { p := fix("x"); p.Meta.Owner.Kind = ""; return p }(),
			want: "owner.kind required",
		},
		{
			name: "host owner needs its host",
			p:    func() *Policy { p := fix("x"); p.Meta.Owner = meta.Owner{Kind: meta.OwnerHost}; return p }(),
			want: "owner.id is required",
		},
		{
			name: "unparseable model ref",
			p:    func() *Policy { p := fix("x"); p.Spec.Models = []string{"a/b/c@"}; return p }(),
			want: "policy \"x\"",
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

// A policy names providers and their models; hosts are the pools' business.
func TestValidateRefusesHosts(t *testing.T) {
	for _, ref := range []string{"anthropic/claude-opus-4-7@bedrock", "@bedrock", "anthropic@bedrock"} {
		p := fix("x")
		p.Spec.Models = []string{ref}
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "names a host") {
			t.Errorf("models [%s]: Validate() = %v, want a refusal naming the host", ref, err)
		}
	}
	p := fix("x")
	p.Spec.Models = []string{"anthropic", "anthropic/claude-opus-4-7"}
	if err := p.Validate(); err != nil {
		t.Errorf("provider and model refs: %v", err)
	}
}
