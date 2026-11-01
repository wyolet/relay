package account

import (
	"strings"
	"testing"

	"github.com/wyolet/relay/app/meta"
)

func fix() *Account {
	return &Account{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-openai", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: meta.NewID()}},
		Spec: Spec{HostID: meta.NewID(), OrgID: "org-abc123"},
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(a *Account)
		want string // "" = valid
	}{
		{name: "team owner with org id"},
		{name: "org id not known yet", edit: func(a *Account) { a.Spec.OrgID = "" }},
		{name: "system owner", edit: func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerSystem} }},
		{name: "user owner", edit: func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerUser, ID: meta.NewID()} }},
		{name: "project owner", edit: func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerProject, ID: meta.NewID()} }},
		{name: "host required", edit: func(a *Account) { a.Spec.HostID = "" }, want: "HostID"},
		{name: "host must be an id", edit: func(a *Account) { a.Spec.HostID = "openai" }, want: "HostID"},
		{name: "team owner without id", edit: func(a *Account) { a.Meta.Owner.ID = "" }, want: "requires an id"},
		{name: "host owner rejected", edit: func(a *Account) { a.Meta.Owner = meta.Owner{Kind: meta.OwnerHost, ID: meta.NewID()} }, want: "owner.kind must be"},
		{name: "missing owner rejected", edit: func(a *Account) { a.Meta.Owner = meta.Owner{} }, want: "owner.kind must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := fix()
			if tc.edit != nil {
				tc.edit(a)
			}
			err := a.Validate()
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
