// Package seed orchestrates loading YAML config into Postgres for the app/
// arch. The flow:
//
//  1. Parse every YAML in the source dir (multi-doc supported).
//  2. List every row from each entity store to learn existing name→id bindings.
//  3. For YAML names that have no PG row yet, mint a fresh UUIDv7 and add it
//     to the resolver map.
//  4. Translate each DTO → domain object via manifest.ToXxx using the merged
//     resolver, stamping Meta.ID from the same map.
//  5. Upsert in dependency order: Provider, Host, RateLimit, HostKey, Model,
//     Policy, RelayKey.
//
// Diffing, dry-run, and identity (User) handling are deliberately omitted —
// this is the minimal "make the rows be there" path. Idempotent: re-running
// over the same YAML is a no-op for unchanged rows and an in-place update
// for changed ones.
package seed

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Options configures a seed run.
type Options struct {
	Pool      *pgxpool.Pool
	YAMLDir   string
	MasterKey []byte // for stored-mode HostKey rows; nil disables them
}

// Result summarises a seed run.
type Result struct {
	Providers  int
	Hosts      int
	RateLimits int
	HostKeys   int
	Models     int
	Pricings   int
	Policies   int
	RelayKeys  int
}

// Run executes the seed pipeline end-to-end.
func Run(ctx context.Context, opts Options) (*Result, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("seed: Pool is required")
	}
	if opts.YAMLDir == "" {
		return nil, fmt.Errorf("seed: YAMLDir is required")
	}

	docs, err := manifest.LoadDir(opts.YAMLDir)
	if err != nil {
		return nil, fmt.Errorf("seed: load yaml: %w", err)
	}

	q := gen.New(opts.Pool)
	stores := storeSet{
		provider:  provider.NewStore(q),
		host:      host.NewStore(q),
		ratelimit: ratelimit.NewStore(q),
		hostkey:   hostkey.NewStore(q, opts.Pool, opts.MasterKey),
		model:     model.NewStore(q),
		policy:    policy.NewStore(opts.Pool),
		pricing:   pricing.NewStore(opts.Pool),
		relaykey:  relaykey.NewStore(q),
	}

	resolver, err := buildResolver(ctx, stores)
	if err != nil {
		return nil, err
	}

	// Bucket docs by kind so we can upsert in dependency order.
	var (
		provDocs []*manifest.ProviderDTO
		hostDocs []*manifest.HostDTO
		rlDocs   []*manifest.RateLimitDTO
		hkDocs   []*manifest.HostKeyDTO
		mDocs    []*manifest.ModelDTO
		prDocs   []*manifest.PricingDTO
		polDocs  []*manifest.PolicyDTO
		rkDocs   []*manifest.RelayKeyDTO
	)
	for _, d := range docs {
		switch {
		case d.Provider != nil:
			provDocs = append(provDocs, d.Provider)
		case d.Host != nil:
			hostDocs = append(hostDocs, d.Host)
		case d.RateLimit != nil:
			rlDocs = append(rlDocs, d.RateLimit)
		case d.HostKey != nil:
			hkDocs = append(hkDocs, d.HostKey)
		case d.Model != nil:
			mDocs = append(mDocs, d.Model)
		case d.Pricing != nil:
			prDocs = append(prDocs, d.Pricing)
		case d.Policy != nil:
			polDocs = append(polDocs, d.Policy)
		case d.RelayKey != nil:
			rkDocs = append(rkDocs, d.RelayKey)
		}
	}

	// Mint ids for any names not yet in PG. Doing this before translate so the
	// resolver is complete when cross-refs are resolved.
	mintIDs(resolver.Providers, provDocs, func(d *manifest.ProviderDTO) string { return d.Metadata.Name })
	mintIDs(resolver.Hosts, hostDocs, func(d *manifest.HostDTO) string { return d.Metadata.Name })
	mintIDs(resolver.RateLimits, rlDocs, func(d *manifest.RateLimitDTO) string { return d.Metadata.Name })
	mintIDs(resolver.HostKeys, hkDocs, func(d *manifest.HostKeyDTO) string { return d.Metadata.Name })
	mintIDs(resolver.Models, mDocs, func(d *manifest.ModelDTO) string { return d.Metadata.Name })
	mintIDs(resolver.Pricings, prDocs, func(d *manifest.PricingDTO) string { return d.Metadata.Name })
	mintIDs(resolver.Policies, polDocs, func(d *manifest.PolicyDTO) string { return d.Metadata.Name })
	mintIDs(resolver.RelayKeys, rkDocs, func(d *manifest.RelayKeyDTO) string { return d.Metadata.Name })

	res := &Result{}

	for _, d := range provDocs {
		p, err := manifest.ToProvider(*d, resolver)
		if err != nil {
			return nil, fmt.Errorf("seed: provider %q: %w", d.Metadata.Name, err)
		}
		p.Meta.ID = resolver.Providers[d.Metadata.Name]
		if err := stores.provider.Upsert(ctx, p); err != nil {
			return nil, fmt.Errorf("seed: upsert provider %q: %w", d.Metadata.Name, err)
		}
		res.Providers++
	}
	for _, d := range hostDocs {
		h, err := manifest.ToHost(*d, resolver)
		if err != nil {
			return nil, fmt.Errorf("seed: host %q: %w", d.Metadata.Name, err)
		}
		h.Meta.ID = resolver.Hosts[d.Metadata.Name]
		if err := stores.host.Upsert(ctx, h); err != nil {
			return nil, fmt.Errorf("seed: upsert host %q: %w", d.Metadata.Name, err)
		}
		res.Hosts++
	}
	for _, d := range rlDocs {
		rl, err := manifest.ToRateLimit(*d, resolver)
		if err != nil {
			return nil, fmt.Errorf("seed: ratelimit %q: %w", d.Metadata.Name, err)
		}
		rl.Meta.ID = resolver.RateLimits[d.Metadata.Name]
		if err := stores.ratelimit.Upsert(ctx, rl); err != nil {
			return nil, fmt.Errorf("seed: upsert ratelimit %q: %w", d.Metadata.Name, err)
		}
		res.RateLimits++
	}
	for _, d := range hkDocs {
		k, err := manifest.ToHostKey(*d, resolver)
		if err != nil {
			return nil, fmt.Errorf("seed: hostkey %q: %w", d.Metadata.Name, err)
		}
		k.Meta.ID = resolver.HostKeys[d.Metadata.Name]
		if err := stores.hostkey.Upsert(ctx, k); err != nil {
			return nil, fmt.Errorf("seed: upsert hostkey %q: %w", d.Metadata.Name, err)
		}
		res.HostKeys++
	}
	for _, d := range mDocs {
		m, err := manifest.ToModel(*d, resolver)
		if err != nil {
			return nil, fmt.Errorf("seed: model %q: %w", d.Metadata.Name, err)
		}
		m.Meta.ID = resolver.Models[d.Metadata.Name]
		if err := stores.model.Upsert(ctx, m); err != nil {
			return nil, fmt.Errorf("seed: upsert model %q: %w", d.Metadata.Name, err)
		}
		res.Models++
	}
	for _, d := range prDocs {
		p, err := manifest.ToPricing(*d, resolver)
		if err != nil {
			return nil, fmt.Errorf("seed: pricing %q: %w", d.Metadata.Name, err)
		}
		p.Meta.ID = resolver.Pricings[d.Metadata.Name]
		if err := stores.pricing.Upsert(ctx, p); err != nil {
			return nil, fmt.Errorf("seed: upsert pricing %q: %w", d.Metadata.Name, err)
		}
		res.Pricings++
	}
	for _, d := range polDocs {
		p, err := manifest.ToPolicy(*d, resolver)
		if err != nil {
			return nil, fmt.Errorf("seed: policy %q: %w", d.Metadata.Name, err)
		}
		p.Meta.ID = resolver.Policies[d.Metadata.Name]
		if err := stores.policy.Upsert(ctx, p); err != nil {
			return nil, fmt.Errorf("seed: upsert policy %q: %w", d.Metadata.Name, err)
		}
		res.Policies++
	}
	for _, d := range rkDocs {
		k, err := manifest.ToRelayKey(*d, resolver)
		if err != nil {
			return nil, fmt.Errorf("seed: relaykey %q: %w", d.Metadata.Name, err)
		}
		k.Meta.ID = resolver.RelayKeys[d.Metadata.Name]
		if err := stores.relaykey.Upsert(ctx, k); err != nil {
			return nil, fmt.Errorf("seed: upsert relaykey %q: %w", d.Metadata.Name, err)
		}
		res.RelayKeys++
	}

	return res, nil
}

// storeSet bundles the seven entity stores so the orchestration code
// doesn't need to thread each individually.
type storeSet struct {
	provider  *provider.Store
	host      *host.Store
	ratelimit *ratelimit.Store
	hostkey   *hostkey.Store
	model     *model.Store
	policy    *policy.Store
	pricing   *pricing.Store
	relaykey  *relaykey.Store
}

// indexBuilder is a mutable resolver populated from PG + freshly minted ids.
// It satisfies manifest.Resolver directly.
type indexBuilder struct {
	Providers  map[string]string
	Hosts      map[string]string
	RateLimits map[string]string
	HostKeys   map[string]string
	Models     map[string]string
	Pricings   map[string]string
	Policies   map[string]string
	RelayKeys  map[string]string
}

func (i *indexBuilder) ProviderID(n string) (string, bool)  { v, ok := i.Providers[n]; return v, ok }
func (i *indexBuilder) HostID(n string) (string, bool)      { v, ok := i.Hosts[n]; return v, ok }
func (i *indexBuilder) PolicyID(n string) (string, bool)    { v, ok := i.Policies[n]; return v, ok }
func (i *indexBuilder) ModelID(n string) (string, bool)     { v, ok := i.Models[n]; return v, ok }
func (i *indexBuilder) HostKeyID(n string) (string, bool)   { v, ok := i.HostKeys[n]; return v, ok }
func (i *indexBuilder) RateLimitID(n string) (string, bool) { v, ok := i.RateLimits[n]; return v, ok }
func (i *indexBuilder) PricingID(n string) (string, bool)   { v, ok := i.Pricings[n]; return v, ok }

func buildResolver(ctx context.Context, s storeSet) (*indexBuilder, error) {
	idx := &indexBuilder{
		Providers:  map[string]string{},
		Hosts:      map[string]string{},
		RateLimits: map[string]string{},
		HostKeys:   map[string]string{},
		Models:     map[string]string{},
		Pricings:   map[string]string{},
		Policies:   map[string]string{},
		RelayKeys:  map[string]string{},
	}
	provs, err := s.provider.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: list providers: %w", err)
	}
	for _, p := range provs {
		idx.Providers[p.Meta.Name] = p.Meta.ID
	}
	hosts, err := s.host.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: list hosts: %w", err)
	}
	for _, h := range hosts {
		idx.Hosts[h.Meta.Name] = h.Meta.ID
	}
	rls, err := s.ratelimit.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: list ratelimits: %w", err)
	}
	for _, r := range rls {
		idx.RateLimits[r.Meta.Name] = r.Meta.ID
	}
	hks, err := s.hostkey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: list hostkeys: %w", err)
	}
	for _, k := range hks {
		idx.HostKeys[k.Meta.Name] = k.Meta.ID
	}
	models, err := s.model.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: list models: %w", err)
	}
	for _, m := range models {
		idx.Models[m.Meta.Name] = m.Meta.ID
	}
	prs, err := s.pricing.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: list pricings: %w", err)
	}
	for _, p := range prs {
		idx.Pricings[p.Meta.Name] = p.Meta.ID
	}
	pols, err := s.policy.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: list policies: %w", err)
	}
	for _, p := range pols {
		idx.Policies[p.Meta.Name] = p.Meta.ID
	}
	rks, err := s.relaykey.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("seed: list relaykeys: %w", err)
	}
	for _, k := range rks {
		idx.RelayKeys[k.Meta.Name] = k.Meta.ID
	}
	return idx, nil
}

func mintIDs[T any](into map[string]string, docs []T, name func(T) string) {
	for _, d := range docs {
		n := name(d)
		if _, ok := into[n]; !ok {
			into[n] = meta.NewID()
		}
	}
}
