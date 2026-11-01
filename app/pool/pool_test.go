package pool

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
)

func fix() *Pool {
	return &Pool{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "openai-main", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}},
		Spec: Spec{HostID: meta.NewID(), CredentialIDs: []string{meta.NewID(), meta.NewID()}},
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(p *Pool)
		want string // "" = valid
	}{
		{name: "two keys, default selection"},
		{name: "round robin", edit: func(p *Pool) { p.Spec.Selection = keypool.KeySelectionRoundRobin }},
		{name: "least recently used", edit: func(p *Pool) { p.Spec.Selection = keypool.KeySelectionLeastRecentlyUsed }},
		{name: "no keys", edit: func(p *Pool) { p.Spec.CredentialIDs = nil }},
		{name: "system owner", edit: func(p *Pool) { p.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }},
		{name: "user owner", edit: func(p *Pool) { p.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()} }},
		{name: "project owner", edit: func(p *Pool) { p.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()} }},
		{name: "host required", edit: func(p *Pool) { p.Spec.HostID = "" }, want: "HostID"},
		{name: "key must be an id", edit: func(p *Pool) { p.Spec.CredentialIDs = []string{"openai-key-a"} }, want: "CredentialIDs"},
		{name: "key listed twice", edit: func(p *Pool) { p.Spec.CredentialIDs[1] = p.Spec.CredentialIDs[0] }, want: "duplicate credentialIds"},
		{name: "unknown selection", edit: func(p *Pool) { p.Spec.Selection = "random" }, want: "Selection"},
		{name: "team owner without id", edit: func(p *Pool) { p.Meta.Owner.ID = "" }, want: "requires an id"},
		{name: "host owner rejected", edit: func(p *Pool) { p.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()} }, want: "owner.kind must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fix()
			if tc.edit != nil {
				tc.edit(p)
			}
			err := p.Validate()
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

func TestDefaultFillsSelection(t *testing.T) {
	p := fix()
	p.Default()
	if p.Spec.Selection != keypool.KeySelectionPrioritized {
		t.Fatalf("default selection = %q, want prioritized", p.Spec.Selection)
	}
	p.Spec.Selection = keypool.KeySelectionRoundRobin
	p.Default()
	if p.Spec.Selection != keypool.KeySelectionRoundRobin {
		t.Fatalf("Default overwrote an explicit selection: %q", p.Spec.Selection)
	}
	var none *Pool
	none.Default()
}
