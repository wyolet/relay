package apply

import (
	"context"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// refs is the control API's cross-row rule set over this run's rows: the
// stored ones, overlaid with what the bundle declares.
func (b *builder) refs() refcheck.Checker {
	return refcheck.Checker{
		Authz: b.opts.Authz,
		Rows: refcheck.Lookup{
			Policy:         lookup(b, b.rows.Policies, func(x *policy.Policy) *meta.Metadata { return &x.Meta }),
			RateLimit:      lookup(b, b.rows.RateLimits, func(x *ratelimit.RateLimit) *meta.Metadata { return &x.Meta }),
			HostKey:        lookup(b, b.rows.HostKeys, func(x *hostkey.HostKey) *meta.Metadata { return &x.Meta }),
			Host:           lookup(b, b.rows.Hosts, func(x *host.Host) *meta.Metadata { return &x.Meta }),
			Project:        lookup(b, b.rows.Projects, func(x *project.Project) *meta.Metadata { return &x.Meta }),
			Team:           lookup(b, b.rows.Teams, func(x *team.Team) *meta.Metadata { return &x.Meta }),
			Role:           lookup(b, b.rows.Roles, func(x *role.Role) *meta.Metadata { return &x.Meta }),
			ServiceAccount: lookup(b, b.rows.ServiceAccounts, func(x *serviceaccount.ServiceAccount) *meta.Metadata { return &x.Meta }),
			MissingUsers:   b.missingUsers,
		},
	}
}

// refsFor returns the checker for a kind's Check hook, or nil without an
// authorizer: the boot seed and CLI load the operator's own tree.
func refsFor[T any](b *builder, check func(refcheck.Checker, context.Context, *T) error) func(context.Context, *T) error {
	if b.opts.Authz == nil {
		return nil
	}
	c := b.refs()
	return func(ctx context.Context, row *T) error { return check(c, ctx, row) }
}

func lookup[T any](b *builder, stored []*T, metaOf func(*T) *meta.Metadata) func(context.Context, string) *T {
	byID := make(map[string]*T, len(stored))
	for _, row := range stored {
		byID[metaOf(row).ID] = row
	}
	return func(_ context.Context, id string) *T {
		if v, ok := b.declared[id].(*T); ok {
			return v
		}
		return byID[id]
	}
}

func (b *builder) missingUsers(_ context.Context, ids []string) ([]string, error) {
	known := make(map[string]bool, len(b.rows.Users))
	for _, u := range b.rows.Users {
		known[u.ID] = true
	}
	var missing []string
	for _, id := range ids {
		if !known[id] {
			missing = append(missing, id)
		}
	}
	return missing, nil
}
