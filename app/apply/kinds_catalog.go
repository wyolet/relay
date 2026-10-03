package apply

import (
	"context"
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/refcheck"
)

func (b *builder) planCatalog(ctx context.Context, provDocs []*manifest.ProviderDTO, hostDocs []*manifest.HostDTO,
	rlDocs []*manifest.RateLimitDTO, hkDocs []*manifest.HostKeyDTO, mDocs []*manifest.ModelDTO,
	prDocs []*manifest.PricingDTO, bndDocs []*manifest.HostBindingDTO, polDocs []*manifest.PolicyDTO) error {
	s := b.opts.Stores
	if err := planKind(ctx, b, kindWiring[manifest.ProviderDTO, provider.Provider]{
		Kind: "Provider", Docs: provDocs, Names: b.idx.Providers, Rows: b.rows.Providers,
		To: manifest.ToProvider, Meta: func(p *provider.Provider) *meta.Metadata { return &p.Meta },
		Upsert: s.Provider.Upsert, Delete: s.Provider.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.HostDTO, host.Host]{
		Kind: "Host", Docs: hostDocs, Names: b.idx.Hosts, Rows: b.rows.Hosts,
		To: manifest.ToHost, Meta: func(h *host.Host) *meta.Metadata { return &h.Meta },
		Upsert: s.Host.Upsert, Delete: s.Host.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.RateLimitDTO, ratelimit.RateLimit]{
		Kind: "RateLimit", Docs: rlDocs, Names: b.idx.RateLimits, Rows: b.rows.RateLimits,
		To: manifest.ToRateLimit, Meta: func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta },
		Upsert: s.RateLimit.Upsert, Delete: detachRefsThenDelete(s.Policy, policy.DetachRateLimit, s.RateLimit.Delete),
	}); err != nil {
		return err
	}
	pols, err := b.policyByID(polDocs)
	if err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.HostKeyDTO, hostkey.HostKey]{
		Kind: "HostKey", Docs: hkDocs, Names: b.idx.HostKeys, Rows: b.rows.HostKeys,
		To: manifest.ToHostKey, Meta: func(k *hostkey.HostKey) *meta.Metadata { return &k.Meta },
		Upsert: s.HostKey.Upsert, Delete: detachRefsThenDelete(s.Policy, policy.DetachHostKey, s.HostKey.Delete),
		Check: checkHostKeyPolicy(b.opts.Authz, pols), Keep: keepHostKeySecret,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.ModelDTO, model.Model]{
		Kind: "Model", Docs: mDocs, Names: b.idx.Models, Rows: b.rows.Models,
		To: manifest.ToModel, Meta: func(m *model.Model) *meta.Metadata { return &m.Meta },
		Upsert: s.Model.Upsert, Delete: s.Model.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.PricingDTO, pricing.Pricing]{
		Kind: "Pricing", Docs: prDocs, Names: b.idx.Pricings, Rows: b.rows.Pricings,
		To: manifest.ToPricing, Meta: func(p *pricing.Pricing) *meta.Metadata { return &p.Meta },
		Upsert: s.Pricing.Upsert, Delete: s.Pricing.Delete,
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.HostBindingDTO, binding.Binding]{
		Kind: "HostBinding", Docs: bndDocs, Names: b.idx.Bindings, Rows: b.rows.Bindings,
		To: manifest.ToHostBinding, Meta: func(x *binding.Binding) *meta.Metadata { return &x.Meta },
		Upsert: s.HostBinding.Upsert, Delete: s.HostBinding.Delete,
		Check: refsFor(b, refcheck.Checker.HostBinding),
	}); err != nil {
		return err
	}
	if err := planKind(ctx, b, kindWiring[manifest.PolicyDTO, policy.Policy]{
		Kind: "Policy", Docs: polDocs, Names: b.idx.Policies, Rows: b.rows.Policies,
		To: manifest.ToPolicy, Meta: func(p *policy.Policy) *meta.Metadata { return &p.Meta },
		Upsert: s.Policy.Upsert, Delete: deletePolicyWithDetach(s),
		Check: refsFor(b, refcheck.Checker.Policy),
	}); err != nil {
		return err
	}
	return nil
}

// deletePolicyWithDetach runs the same reference cleanup, and the same
// refusal, the control API's delete path runs. Pruning a policy without them
// leaves keys and service accounts pointing at a row that is gone, or host
// keys with no tier policy at all.
func deletePolicyWithDetach(s *Stores) func(context.Context, string) error {
	return detachThenDelete(policy.DetachStores{
		Keys:            s.Key,
		ServiceAccounts: s.ServiceAccount,
		HostKeys:        s.HostKey,
		Hosts:           s.Host,
	}, s.Policy.Delete)
}

func detachThenDelete(refs policy.DetachStores, del func(context.Context, string) error) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		// A tier policy cannot be removed while host keys mirror it: their
		// policyId is required, so there is no valid row to leave behind.
		names, err := policy.HostKeysUsingPolicy(ctx, refs, id)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			return fmt.Errorf("policy %s is the tier policy of host key(s) %s: reattach them before pruning it",
				id, strings.Join(names, ", "))
		}
		if err := policy.Detach(ctx, refs, id); err != nil {
			return err
		}
		return del(ctx, id)
	}
}

// keepHostKeySecret keeps the stored secret of a stored or oauth host key
// whose document carries no value, as an export does; Store.Upsert leaves
// the ciphertext untouched when it sees Resolved and no Value.
func keepHostKeySecret(prev, next *hostkey.HostKey) {
	kind := next.Spec.ValueFrom.Kind
	if next.Spec.Value != "" || kind != prev.Spec.ValueFrom.Kind ||
		(kind != hostkey.ValueKindStored && kind != hostkey.ValueKindOAuth) {
		return
	}
	next.Resolved = prev.Resolved
}

// detachRefsThenDelete strips the policies' references to a pruned host key
// or rate limit before deleting it, as the control API's delete does.
func detachRefsThenDelete(pols policy.Policies, detach func(context.Context, policy.Policies, string) error,
	del func(context.Context, string) error) func(context.Context, string) error {
	return func(ctx context.Context, id string) error {
		if err := detach(ctx, pols, id); err != nil {
			return err
		}
		return del(ctx, id)
	}
}

// policyByID indexes every Policy this run can resolve: the stored rows plus
// the ones the same bundle declares, so a key may name a tier policy created
// by the same apply.
func (b *builder) policyByID(docs []*manifest.PolicyDTO) (map[string]*policy.Policy, error) {
	out := make(map[string]*policy.Policy, len(b.rows.Policies)+len(docs))
	for _, p := range b.rows.Policies {
		out[p.Meta.ID] = p
	}
	for _, d := range docs {
		p, err := manifest.ToPolicy(*d, b.idx)
		if err != nil {
			return nil, fmt.Errorf("apply: Policy %q: %w", d.Metadata.Name, err)
		}
		p.Meta.ID = b.idx.Policies[d.Metadata.Name]
		out[p.Meta.ID] = p
	}
	return out, nil
}

// checkHostKeyPolicy runs the API's host-key rule in every load, the boot
// seed included: a key whose tier policy is not host-owned by its own host
// drops out of the snapshot. pols covers tier policies the same bundle
// declares, which plan after host keys.
func checkHostKeyPolicy(a authz.Authorizer, pols map[string]*policy.Policy) func(context.Context, *hostkey.HostKey) error {
	c := refcheck.Checker{Authz: a, Rows: refcheck.Lookup{
		Policy: func(_ context.Context, id string) *policy.Policy { return pols[id] },
	}}
	return c.HostKey
}
