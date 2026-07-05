package catalog

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/pkg/slug"
)

// audit 2026-07-04 [P2 RESTRUCTURE]: provider rename leaves synthesized
// snapshot aliases stale on the incremental Apply path.
func TestApply_ProviderRenameReindexesSnapshotAliases(t *testing.T) {
	t.Skip("audit 2026-07-04: provider/host rename leaves snapshot aliases stale — known-broken, unskip with the fix")
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	snapName := "gpt-4o-2025-01-01"
	if _, _, _, ok := c.Current().ResolveSnapshot(slug.From("openai/" + snapName)); !ok {
		t.Fatal("precondition: provider-qualified alias not resolvable before rename")
	}

	renamed := &provider.Provider{
		Meta: meta.Metadata{ID: provs[0].Meta.ID, Name: "openai-renamed", Owner: provs[0].Meta.Owner},
		Spec: provs[0].Spec,
	}
	if err := c.ApplyProviderUpsert(renamed); err != nil {
		t.Fatalf("ApplyProviderUpsert: %v", err)
	}

	s := c.Current()
	if _, ok := s.ProviderByName("openai-renamed"); !ok {
		t.Fatal("renamed provider missing by new slug")
	}
	if _, _, _, ok := s.ResolveSnapshot(slug.From("openai-renamed/" + snapName)); !ok {
		t.Errorf("new-slug alias %q does not resolve after provider rename", "openai-renamed/"+snapName)
	}
	if _, _, _, ok := s.ResolveSnapshot(slug.From("openai/" + snapName)); ok {
		t.Errorf("old-slug alias %q still resolves after provider rename", "openai/"+snapName)
	}
}

// audit 2026-07-04 [P2 RESTRUCTURE]: host rename leaves synthesized
// host-pinned snapshot aliases stale on the incremental Apply path.
func TestApply_HostRenameReindexesSnapshotAliases(t *testing.T) {
	t.Skip("audit 2026-07-04: provider/host rename leaves snapshot aliases stale — known-broken, unskip with the fix")
	provs, hosts, pols, models, keys, rls, rks, bnds := fixture()
	c := New(provs, hosts, pols, models, keys, rls, rks, rcList{}, bnds)
	if err := c.Reload(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}

	snapName := "gpt-4o-2025-01-01"
	if _, _, _, ok := c.Current().ResolveSnapshot(slug.From(snapName + "@openai-direct")); !ok {
		t.Fatal("precondition: host-pinned alias not resolvable before rename")
	}

	renamed := &host.Host{
		Meta: meta.Metadata{ID: hosts[0].Meta.ID, Name: "openai-direct-renamed", Owner: hosts[0].Meta.Owner},
		Spec: hosts[0].Spec,
	}
	if err := c.ApplyHostUpsert(renamed); err != nil {
		t.Fatalf("ApplyHostUpsert: %v", err)
	}

	s := c.Current()
	if _, ok := s.HostByName("openai-direct-renamed"); !ok {
		t.Fatal("renamed host missing by new slug")
	}
	if _, _, _, ok := s.ResolveSnapshot(slug.From(snapName + "@openai-direct-renamed")); !ok {
		t.Errorf("new-slug alias %q does not resolve after host rename", snapName+"@openai-direct-renamed")
	}
	if _, _, _, ok := s.ResolveSnapshot(slug.From(snapName + "@openai-direct")); ok {
		t.Errorf("old-slug alias %q still resolves after host rename", snapName+"@openai-direct")
	}
}
