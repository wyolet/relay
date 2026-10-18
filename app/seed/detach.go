package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/wyolet/relay/app/apply"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
)

// detachDropped marks the catalog rows the seeded tree no longer declares as
// detached, and clears the mark on declared rows that came back and are not
// operator-edited. It runs after a successful Execute, so the plan names every
// row the tree carries. Each write goes through the kind's store Upsert with
// the version just read, so a row edited meanwhile fails the pass instead of
// being overwritten.
func detachDropped(ctx context.Context, stores *apply.Stores, plan *apply.Result, now time.Time, res *Result) error {
	declared := map[string]map[string]bool{}
	for _, e := range plan.Entries {
		if declared[e.Kind] == nil {
			declared[e.Kind] = map[string]bool{}
		}
		declared[e.Kind][e.Name] = true
	}
	rows, err := apply.Load(ctx, stores)
	if err != nil {
		return err
	}
	o := origin{providers: map[string]meta.Owner{}, hosts: map[string]meta.Owner{}}
	for _, p := range rows.Providers {
		o.providers[p.Meta.ID] = p.Meta.Owner
	}
	for _, h := range rows.Hosts {
		o.hosts[h.Meta.ID] = h.Meta.Owner
	}
	m := marker{declared: declared, origin: o, now: now, res: res}

	if err := markRows(ctx, m, "Provider", rows.Providers,
		func(x *provider.Provider) *meta.Metadata { return &x.Meta }, stores.Provider.Upsert); err != nil {
		return err
	}
	if err := markRows(ctx, m, "Host", rows.Hosts,
		func(x *host.Host) *meta.Metadata { return &x.Meta }, stores.Host.Upsert); err != nil {
		return err
	}
	if err := markRows(ctx, m, "RateLimit", rows.RateLimits,
		func(x *ratelimit.RateLimit) *meta.Metadata { return &x.Meta }, stores.RateLimit.Upsert); err != nil {
		return err
	}
	if err := markRows(ctx, m, "Model", rows.Models,
		func(x *model.Model) *meta.Metadata { return &x.Meta }, stores.Model.Upsert); err != nil {
		return err
	}
	if err := markRows(ctx, m, "Pricing", rows.Pricings,
		func(x *pricing.Pricing) *meta.Metadata { return &x.Meta }, stores.Pricing.Upsert); err != nil {
		return err
	}
	if err := markRows(ctx, m, "HostBinding", rows.Bindings,
		func(x *binding.Binding) *meta.Metadata { return &x.Meta }, stores.HostBinding.Upsert); err != nil {
		return err
	}
	return markRows(ctx, m, "Policy", rows.Policies,
		func(x *policy.Policy) *meta.Metadata { return &x.Meta }, stores.Policy.Upsert)
}

// marker carries what one detach pass decides by: the names the tree
// declares per kind, the row origin rule, the timestamp to stamp, and the
// counts to report.
type marker struct {
	declared map[string]map[string]bool
	origin   origin
	now      time.Time
	res      *Result
}

// markRows detaches the rows of one kind the tree dropped and re-attaches the
// clean ones it declares again. A dirty declared row keeps its mark: the seed
// skipped it, so it is still the operator's copy.
func markRows[T any](ctx context.Context, mk marker, kind string, rows []*T,
	metaOf func(*T) *meta.Metadata, upsert func(context.Context, *T) error) error {
	for _, row := range rows {
		m := metaOf(row)
		switch {
		case mk.declared[kind][m.Name]:
			if m.CatalogDetachedAt == nil || m.Dirty {
				continue
			}
			m.CatalogDetachedAt = nil
			mk.res.Reattached++
		case m.CatalogDetachedAt == nil && mk.origin.fromCatalog(kind, m):
			at := mk.now
			m.CatalogDetachedAt = &at
			mk.res.Detached++
		default:
			continue
		}
		if err := upsert(ctx, row); err != nil {
			return fmt.Errorf("mark %s %q: %w", kind, m.Name, err)
		}
	}
	return nil
}

// origin decides whether a stored row came from a catalog tree. No row
// records that, so it is inferred the way governance infers catalog-managed
// rows: a system, provider or host owner, and for the latter two a Provider
// or Host that is itself catalog-owned.
type origin struct {
	providers map[string]meta.Owner
	hosts     map[string]meta.Owner
}

func (o origin) fromCatalog(kind string, m *meta.Metadata) bool {
	switch m.Owner.Kind {
	case meta.OwnerSystem:
		// System rate limits are the relay's own rows, created at boot from
		// its built-in definitions, never catalog data.
		return kind != "RateLimit"
	case meta.OwnerProvider:
		parent, ok := o.providers[m.Owner.ID]
		return ok && catalogOwner(parent)
	case meta.OwnerHost:
		parent, ok := o.hosts[m.Owner.ID]
		return ok && catalogOwner(parent)
	}
	return false
}

// catalogOwner reports whether a Provider or Host row is catalog-owned. A
// host created through the API carries no owner kind, and its tiers are the
// operator's.
func catalogOwner(o meta.Owner) bool {
	switch o.Kind {
	case meta.OwnerSystem, meta.OwnerProvider, meta.OwnerHost:
		return true
	}
	return false
}
