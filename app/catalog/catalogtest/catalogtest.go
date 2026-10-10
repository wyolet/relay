// Package catalogtest builds a catalog.Catalog or catalog.Snapshot from rows
// held in memory, for tests outside app/catalog. It carries no fixtures of its
// own: each test states the rows it needs. Tests inside app/catalog cannot
// import it, since the package imports app/catalog.
package catalogtest

import (
	"context"
	"testing"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// Rows lists a fixed slice, so it satisfies every catalog Lister.
type Rows[T any] []*T

func (r Rows[T]) List(context.Context) ([]*T, error) { return r, nil }

// Catalog is the rows a test catalog holds, one field per kind. A nil field
// is an empty kind.
type Catalog struct {
	Providers  []*provider.Provider
	Hosts      []*host.Host
	Policies   []*policy.Policy
	Models     []*model.Model
	HostKeys   []*hostkey.HostKey
	RateLimits []*ratelimit.RateLimit
	Keys       []*key.Key
	Pricings   []*pricing.Pricing
	Bindings   []*binding.Binding

	Teams           []*team.Team
	Projects        []*project.Project
	ServiceAccounts []*serviceaccount.ServiceAccount
	Groups          []*group.Group
	Roles           []*role.Role
	RoleBindings    []*rolebinding.RoleBinding
	PolicyBindings  []*policybinding.PolicyBinding
}

// New returns a catalog over the rows with tenancy attached, before its first
// Reload, for a test that installs a clock or a token-version source first.
func (c Catalog) New() *catalog.Catalog {
	cat := catalog.New(
		Rows[provider.Provider](c.Providers), Rows[host.Host](c.Hosts), Rows[policy.Policy](c.Policies),
		Rows[model.Model](c.Models), Rows[hostkey.HostKey](c.HostKeys), Rows[ratelimit.RateLimit](c.RateLimits),
		Rows[key.Key](c.Keys), Rows[pricing.Pricing](c.Pricings), Rows[binding.Binding](c.Bindings),
	)
	cat.UseTenancy(
		Rows[team.Team](c.Teams), Rows[project.Project](c.Projects),
		Rows[serviceaccount.ServiceAccount](c.ServiceAccounts), Rows[group.Group](c.Groups),
		Rows[role.Role](c.Roles), Rows[rolebinding.RoleBinding](c.RoleBindings),
		Rows[policybinding.PolicyBinding](c.PolicyBindings),
	)
	return cat
}

// Load returns a catalog over the rows after its first Reload.
func (c Catalog) Load(t testing.TB) *catalog.Catalog {
	t.Helper()
	cat := c.New()
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("catalogtest: reload: %v", err)
	}
	return cat
}

// Snapshot assembles the rows with catalog.Build, which skips Reload's
// cross-reference validation. Build takes no tenancy rows, so setting any is
// a test bug and panics; use Load for those.
func (c Catalog) Snapshot() *catalog.Snapshot {
	if c.Teams != nil || c.Projects != nil || c.ServiceAccounts != nil || c.Groups != nil ||
		c.Roles != nil || c.RoleBindings != nil || c.PolicyBindings != nil {
		panic("catalogtest: Snapshot cannot hold tenancy rows; use Load")
	}
	return catalog.Build(c.Providers, c.Hosts, c.Policies, c.Keys, c.Models, c.HostKeys, c.RateLimits, c.Pricings, c.Bindings)
}
