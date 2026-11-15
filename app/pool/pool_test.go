package pool

import (
	"testing"

	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/pkg/meta"
)

const (
	idA = "01a19bc3-0000-7000-8000-000000000001"
	idB = "01a19bc3-0000-7000-8000-000000000002"
)

func fix() *Pool {
	return &Pool{
		Meta: meta.Metadata{ID: idA, Name: "openai-main", Owner: meta.Owner{Kind: meta.OwnerTeam, ID: idA}},
		Spec: Spec{HostID: idA, CredentialIDs: []string{idA, idB}},
	}
}

func TestDefaultFillsSelection(t *testing.T) {
	p := fix()
	p.Default()
	if p.Spec.Selection != keypool.KeySelectionPrioritized {
		t.Errorf("default selection = %q, want prioritized", p.Spec.Selection)
	}
	p.Spec.Selection = keypool.KeySelectionRoundRobin
	p.Default()
	if p.Spec.Selection != keypool.KeySelectionRoundRobin {
		t.Errorf("Default overwrote an explicit selection: %q", p.Spec.Selection)
	}
	var none *Pool
	none.Default()
}

func TestValidate(t *testing.T) {
	owner := func(kind meta.OwnerKind, id string) func(*Pool) {
		return func(p *Pool) { p.Meta.Owner = meta.Owner{Kind: kind, ID: id} }
	}
	for _, tc := range []struct {
		name string
		edit func(p *Pool)
		want string
	}{
		{"two keys, selection left to the default", func(*Pool) {}, ""},
		{"prioritized", func(p *Pool) { p.Spec.Selection = keypool.KeySelectionPrioritized }, ""},
		{"round robin", func(p *Pool) { p.Spec.Selection = keypool.KeySelectionRoundRobin }, ""},
		{"least recently used", func(p *Pool) { p.Spec.Selection = keypool.KeySelectionLeastRecentlyUsed }, ""},
		{"no keys", func(p *Pool) { p.Spec.CredentialIDs = nil }, ""},
		{"system owner", owner(meta.OwnerSystem, ""), ""},
		{"organization owner", owner(meta.OwnerOrganization, ""), ""},
		{"user owner", owner(meta.OwnerUser, idB), ""},
		{"project owner", owner(meta.OwnerProject, idB), ""},
		{"accounts and keys", func(p *Pool) { p.Spec.AccountIDs = []string{idA, idB} }, ""},
		{"one id as an account and as a key", func(p *Pool) { p.Spec.AccountIDs = []string{idA} }, ""},
		{"no host", func(p *Pool) { p.Spec.HostID = "" }, "Key: 'Pool.Spec.HostID' Error:Field validation for 'HostID' failed on the 'required' tag"},
		{"host that is no id", func(p *Pool) { p.Spec.HostID = "openai" }, "Key: 'Pool.Spec.HostID' Error:Field validation for 'HostID' failed on the 'uuid' tag"},
		{"key that is no id", func(p *Pool) { p.Spec.CredentialIDs = []string{idA, "openai-key-a"} }, "Key: 'Pool.Spec.CredentialIDs[1]' Error:Field validation for 'CredentialIDs[1]' failed on the 'uuid' tag"},
		{"key listed twice", func(p *Pool) { p.Spec.CredentialIDs = []string{idA, idB, idA} }, `pool "openai-main": duplicate credentialIds entry "01a19bc3-0000-7000-8000-000000000001"`},
		{"account that is no id", func(p *Pool) { p.Spec.AccountIDs = []string{"openai-org"} }, "Key: 'Pool.Spec.AccountIDs[0]' Error:Field validation for 'AccountIDs[0]' failed on the 'uuid' tag"},
		{"account listed twice", func(p *Pool) { p.Spec.AccountIDs = []string{idB, idB} }, `pool "openai-main": duplicate accountIds entry "01a19bc3-0000-7000-8000-000000000002"`},
		{"unknown selection", func(p *Pool) { p.Spec.Selection = "random" }, "Key: 'Pool.Spec.Selection' Error:Field validation for 'Selection' failed on the 'oneof' tag"},
		{"team owner without id", owner(meta.OwnerTeam, ""), `pool "openai-main": owner.kind "team" requires an id`},
		{"project owner without id", owner(meta.OwnerProject, ""), `pool "openai-main": owner.kind "project" requires an id`},
		{"organization owner with an id", owner(meta.OwnerOrganization, idB), `pool "openai-main": an organization owner carries no id`},
		{"host owner", owner(meta.OwnerHost, idB), `pool "openai-main": owner.kind must be system, organization, team, project or user, got "host"`},
		{"no owner", owner("", ""), `pool "openai-main": owner.kind must be system, organization, team, project or user, got ""`},
	} {
		p := fix()
		tc.edit(p)
		got := ""
		if err := p.Validate(); err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%s:\n got %q\nwant %s", tc.name, got, tc.want)
		}
	}
}
