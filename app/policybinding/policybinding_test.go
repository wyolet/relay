package policybinding

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix() *PolicyBinding {
	return &PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-eval", Owner: meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()}},
		Spec: Spec{PolicyID: meta.NewID(), AccountIDs: []string{meta.NewID()}},
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(b *PolicyBinding)
		want string // "" = valid
	}{
		{name: "project owner"},
		{name: "team owner", edit: func(b *PolicyBinding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()} }},
		{name: "user owner", edit: func(b *PolicyBinding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()} }},
		{name: "system owner", edit: func(b *PolicyBinding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }},
		{name: "several accounts", edit: func(b *PolicyBinding) { b.Spec.AccountIDs = append(b.Spec.AccountIDs, meta.NewID()) }},
		{name: "system owner with an id", edit: func(b *PolicyBinding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem, ID: meta.NewID()} }, want: "has no id"},
		{name: "team owner without an id", edit: func(b *PolicyBinding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerTeam} }, want: "needs its id"},
		{name: "host owner", edit: func(b *PolicyBinding) { b.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()} }, want: "owner.kind must be"},
		{name: "no accounts", edit: func(b *PolicyBinding) { b.Spec.AccountIDs = nil }, want: "AccountIDs"},
		{name: "an account that is not an id", edit: func(b *PolicyBinding) { b.Spec.AccountIDs = []string{"team-ml"} }, want: "AccountIDs"},
		{name: "an account twice", edit: func(b *PolicyBinding) { b.Spec.AccountIDs = append(b.Spec.AccountIDs, b.Spec.AccountIDs[0]) }, want: "listed twice"},
		{name: "policy required", edit: func(b *PolicyBinding) { b.Spec.PolicyID = "" }, want: "PolicyID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fix()
			if tc.edit != nil {
				tc.edit(b)
			}
			err := b.Validate()
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

func TestScopeKey(t *testing.T) {
	b := fix()
	if got, want := b.ScopeKey(), "project:"+b.Meta.Owner.ID; got != want {
		t.Errorf("ScopeKey() = %q, want %q", got, want)
	}
	b.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem}
	if got := b.ScopeKey(); got != "" {
		t.Errorf("system ScopeKey() = %q, want empty", got)
	}
}
