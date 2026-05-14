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
			name: "hostKeyIds non-uuid",
			p: func() *Policy {
				p := fix("x")
				p.Spec.HostKeyIDs = []string{"not-a-uuid"}
				return p
			}(),
			want: "HostKeyIDs",
		},
		{
			name: "modelIds non-uuid",
			p: func() *Policy {
				p := fix("x")
				p.Spec.ModelIDs = []string{"not-a-uuid"}
				return p
			}(),
			want: "ModelIDs",
		},
		{
			name: "duplicate hostKeyIds",
			p: func() *Policy {
				p := fix("x")
				id := meta.NewID()
				p.Spec.HostKeyIDs = []string{id, id}
				return p
			}(),
			want: "duplicate hostKeyIds",
		},
		{
			name: "duplicate modelIds",
			p: func() *Policy {
				p := fix("x")
				id := meta.NewID()
				p.Spec.ModelIDs = []string{id, id}
				return p
			}(),
			want: "duplicate modelIds",
		},
		{
			name: "bad key selection",
			p: func() *Policy {
				p := fix("x")
				p.Spec.KeySelection = "bogus"
				return p
			}(),
			want: "KeySelection",
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestEffectiveKeySelection(t *testing.T) {
	for _, tc := range []struct {
		name string
		ks   KeySelection
		want KeySelection
	}{
		{"unset defaults to prioritized", "", KeySelectionPrioritized},
		{"round-robin keeps", KeySelectionRoundRobin, KeySelectionRoundRobin},
		{"lru keeps", KeySelectionLeastRecentlyUsed, KeySelectionLeastRecentlyUsed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fix("p")
			p.Spec.KeySelection = tc.ks
			if got := p.EffectiveKeySelection(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
