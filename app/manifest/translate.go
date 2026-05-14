package manifest

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

func ToProvider(d ProviderDTO, _ Resolver) (*provider.Provider, error) {
	return &provider.Provider{
		Meta: d.Metadata.toMeta(),
		Spec: provider.Spec{
			Enabled:       d.Spec.Enabled,
			HomepageURL:   d.Spec.HomepageURL,
			DocsURL:       d.Spec.DocsURL,
			StatusPageURL: d.Spec.StatusPageURL,
			Icon:          d.Spec.Icon,
		},
	}, nil
}

func FromProvider(p *provider.Provider, _ ReverseResolver) ProviderDTO {
	return ProviderDTO{
		APIVersion: APIVersion,
		Kind:       "Provider",
		Metadata:   metaToWire(p.Meta),
		Spec: ProviderSpec{
			Enabled:       p.Spec.Enabled,
			HomepageURL:   p.Spec.HomepageURL,
			DocsURL:       p.Spec.DocsURL,
			StatusPageURL: p.Spec.StatusPageURL,
			Icon:          p.Spec.Icon,
		},
	}
}

// ---------------------------------------------------------------------------
// Host
// ---------------------------------------------------------------------------

func ToHost(d HostDTO, idx Resolver) (*host.Host, error) {
	policies := make([]string, 0, len(d.Spec.Policies))
	for _, name := range d.Spec.Policies {
		if id, ok := idx.PolicyID(name); ok {
			policies = append(policies, id)
		} else {
			policies = append(policies, name)
		}
	}
	defaultPolicy := d.Spec.DefaultPolicy
	if defaultPolicy != "" {
		if id, ok := idx.PolicyID(defaultPolicy); ok {
			defaultPolicy = id
		}
	}
	return &host.Host{
		Meta: d.Metadata.toMeta(),
		Spec: host.Spec{
			BaseURL:       d.Spec.BaseURL,
			Backend:       d.Spec.Backend,
			Policies:      policies,
			DefaultPolicy: defaultPolicy,
			Enabled:       d.Spec.Enabled,
			HomepageURL:   d.Spec.HomepageURL,
			DocsURL:       d.Spec.DocsURL,
			ConsoleURL:    d.Spec.ConsoleURL,
			StatusPageURL: d.Spec.StatusPageURL,
			Icon:          d.Spec.Icon,
		},
	}, nil
}

func FromHost(h *host.Host, rev ReverseResolver) HostDTO {
	policies := make([]string, 0, len(h.Spec.Policies))
	for _, id := range h.Spec.Policies {
		if name, ok := rev.PolicyName(id); ok {
			policies = append(policies, name)
		} else {
			policies = append(policies, id)
		}
	}
	defaultPolicy := h.Spec.DefaultPolicy
	if defaultPolicy != "" {
		if name, ok := rev.PolicyName(defaultPolicy); ok {
			defaultPolicy = name
		}
	}
	return HostDTO{
		APIVersion: APIVersion,
		Kind:       "Host",
		Metadata:   metaToWire(h.Meta),
		Spec: HostSpec{
			BaseURL:       h.Spec.BaseURL,
			Backend:       h.Spec.Backend,
			Policies:      policies,
			DefaultPolicy: defaultPolicy,
			Enabled:       h.Spec.Enabled,
			HomepageURL:   h.Spec.HomepageURL,
			DocsURL:       h.Spec.DocsURL,
			ConsoleURL:    h.Spec.ConsoleURL,
			StatusPageURL: h.Spec.StatusPageURL,
			Icon:          h.Spec.Icon,
		},
	}
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

// ToModel resolves host names to ids. The model's owning provider is expressed
// as Owner.ID in the wire metadata; if it's a name (not a UUID), the caller
// should resolve it before calling ToModel or pass it pre-resolved.
//
// Provider owner: the wire form stores the provider *name* in Metadata.Owner.ID
// when coming from YAML. Callers who need name→id resolution for the owner
// should do so before this call, or supply the provider id directly.
func ToModel(d ModelDTO, idx Resolver) (*model.Model, error) {
	m := &model.Model{
		Meta: d.Metadata.toMeta(),
	}

	// Resolve host names → ids for each binding.
	bindings := make([]model.HostBinding, 0, len(d.Spec.Hosts))
	for i, b := range d.Spec.Hosts {
		hostID, ok := idx.HostID(b.Host)
		if !ok {
			return nil, fmt.Errorf("model %q: hosts[%d]: host %q not found", d.Metadata.Name, i, b.Host)
		}
		bindings = append(bindings, model.HostBinding{
			HostID:       hostID,
			UpstreamName: b.UpstreamName,
			Adapter:      adapter.Kind(b.Adapter),
			Enabled:      b.Enabled,
		})
	}

	// Resolve owner provider name → id if the owner kind is provider and the
	// ID looks like a name (not a UUID). We do a best-effort resolution; if it
	// already looks resolved the caller wins.
	ownerID := m.Meta.Owner.ID
	if m.Meta.Owner.Kind == meta.OwnerProvider && ownerID != "" {
		if pid, ok := idx.ProviderID(ownerID); ok {
			m.Meta.Owner.ID = pid
		}
		// else: already an id or caller's responsibility
	}

	m.Spec = model.Spec{
		Hosts:                bindings,
		Family:               d.Spec.Family,
		Version:              d.Spec.Version,
		Capabilities:         d.Spec.Capabilities,
		Modalities:           d.Spec.Modalities,
		ContextWindowInput:   d.Spec.ContextWindowInput,
		ContextWindowOutput:  d.Spec.ContextWindowOutput,
		ContextWindowTotal:   d.Spec.ContextWindowTotal,
		MaxOutputTokens:      d.Spec.MaxOutputTokens,
		KnowledgeCutoff:      d.Spec.KnowledgeCutoff,
		ReleaseDate:          d.Spec.ReleaseDate,
		DeprecationDate:      d.Spec.DeprecationDate,
		Deprecation:          d.Spec.Deprecation,
		Aliases:              d.Spec.Aliases,
		Tags:                 d.Spec.Tags,
		Documentation:        d.Spec.Documentation,
		License:              d.Spec.License,
		ProviderModelPageURL: d.Spec.ProviderModelPageURL,
		Enabled:              d.Spec.Enabled,
	}
	return m, nil
}

func FromModel(m *model.Model, rev ReverseResolver) ModelDTO {
	bindings := make([]HostBindingDTO, 0, len(m.Spec.Hosts))
	for _, b := range m.Spec.Hosts {
		name, _ := rev.HostName(b.HostID)
		if name == "" {
			name = b.HostID // fallback to id
		}
		bindings = append(bindings, HostBindingDTO{
			Host:         name,
			UpstreamName: b.UpstreamName,
			Adapter:      string(b.Adapter),
			Enabled:      b.Enabled,
		})
	}

	wm := metaToWire(m.Meta)
	// Render owner provider id → name
	if m.Meta.Owner.Kind == meta.OwnerProvider && m.Meta.Owner.ID != "" {
		if pname, ok := rev.ProviderName(m.Meta.Owner.ID); ok {
			wm.Owner.Name = pname
		}
	}

	return ModelDTO{
		APIVersion: APIVersion,
		Kind:       "Model",
		Metadata:   wm,
		Spec: ModelSpec{
			Hosts:                bindings,
			Family:               m.Spec.Family,
			Version:              m.Spec.Version,
			Capabilities:         m.Spec.Capabilities,
			Modalities:           m.Spec.Modalities,
			ContextWindowInput:   m.Spec.ContextWindowInput,
			ContextWindowOutput:  m.Spec.ContextWindowOutput,
			ContextWindowTotal:   m.Spec.ContextWindowTotal,
			MaxOutputTokens:      m.Spec.MaxOutputTokens,
			KnowledgeCutoff:      m.Spec.KnowledgeCutoff,
			ReleaseDate:          m.Spec.ReleaseDate,
			DeprecationDate:      m.Spec.DeprecationDate,
			Deprecation:          m.Spec.Deprecation,
			Aliases:              m.Spec.Aliases,
			Tags:                 m.Spec.Tags,
			Documentation:        m.Spec.Documentation,
			License:              m.Spec.License,
			ProviderModelPageURL: m.Spec.ProviderModelPageURL,
			Enabled:              m.Spec.Enabled,
		},
	}
}

// ---------------------------------------------------------------------------
// HostKey
// ---------------------------------------------------------------------------

// ToHostKey resolves Spec.HostID (host name → id).
func ToHostKey(d HostKeyDTO, idx Resolver) (*hostkey.HostKey, error) {
	m := d.Metadata.toMeta()
	if m.Owner.Kind == "" {
		m.Owner.Kind = meta.OwnerSystem
	}
	hostID := d.Spec.HostID
	if hostID != "" {
		if id, ok := idx.HostID(hostID); ok {
			hostID = id
		}
	}
	return &hostkey.HostKey{
		Meta: m,
		Spec: hostkey.Spec{
			HostID: hostID,
			ValueFrom: hostkey.ValueFrom{
				Kind: hostkey.ValueKind(d.Spec.ValueFrom.Kind),
				Env:  d.Spec.ValueFrom.Env,
			},
			DefaultTier: d.Spec.DefaultTier,
			Enabled:     d.Spec.Enabled,
			Value:       d.Spec.Value,
		},
	}, nil
}

func FromHostKey(k *hostkey.HostKey, rev ReverseResolver) HostKeyDTO {
	wm := metaToWire(k.Meta)
	hostID := k.Spec.HostID
	if hostID != "" {
		if hname, ok := rev.HostName(hostID); ok {
			hostID = hname
		}
	}
	return HostKeyDTO{
		APIVersion: APIVersion,
		Kind:       "HostKey",
		Metadata:   wm,
		Spec: HostKeySpec{
			HostID: hostID,
			ValueFrom: HostKeyValueFrom{
				Kind: string(k.Spec.ValueFrom.Kind),
				Env:  k.Spec.ValueFrom.Env,
			},
			DefaultTier: k.Spec.DefaultTier,
			Enabled:     k.Spec.Enabled,
			// Value intentionally omitted — never returned in responses
		},
	}
}

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

func ToPolicy(d PolicyDTO, idx Resolver) (*policy.Policy, error) {
	// Spec.Models entries are modelref DSL strings — stored verbatim on
	// the Policy. Validation (Policy.Validate) re-runs the parser.
	models := make([]string, 0, len(d.Spec.Models))
	models = append(models, d.Spec.Models...)

	hostKeyIDs := make([]string, 0, len(d.Spec.HostKeys))
	for _, name := range d.Spec.HostKeys {
		id, ok := idx.HostKeyID(name)
		if !ok {
			return nil, fmt.Errorf("policy %q: hostKey %q not found", d.Metadata.Name, name)
		}
		hostKeyIDs = append(hostKeyIDs, id)
	}

	var rateLimitID string
	if d.Spec.RateLimit != "" {
		id, ok := idx.RateLimitID(d.Spec.RateLimit)
		if !ok {
			return nil, fmt.Errorf("policy %q: rateLimit %q not found", d.Metadata.Name, d.Spec.RateLimit)
		}
		rateLimitID = id
	}

	m := d.Metadata.toMeta()
	if m.Owner.Kind == meta.OwnerHost && m.Owner.ID != "" {
		if hid, ok := idx.HostID(m.Owner.ID); ok {
			m.Owner.ID = hid
		}
	}
	return &policy.Policy{
		Meta: m,
		Spec: policy.Spec{
			Models:            models,
			HostKeyIDs:        hostKeyIDs,
			RateLimitID:       rateLimitID,
			KeySelection:      policy.KeySelection(d.Spec.KeySelection),
			SkipDefaultLimits: d.Spec.SkipDefaultLimits,
			IncludeDeprecated: d.Spec.IncludeDeprecated,
			Enabled:           d.Spec.Enabled,
		},
	}, nil
}

func FromPolicy(p *policy.Policy, rev ReverseResolver) PolicyDTO {
	// Spec.Models is already in wire form (ref strings). Spec.ModelIDs
	// is the legacy literal-ID grant; emit its rows as bare model names
	// for backward-compat with operator-authored YAML.
	models := make([]string, 0, len(p.Spec.Models)+len(p.Spec.ModelIDs))
	models = append(models, p.Spec.Models...)
	for _, id := range p.Spec.ModelIDs {
		name, _ := rev.ModelName(id)
		if name == "" {
			name = id
		}
		models = append(models, name)
	}

	hostKeys := make([]string, 0, len(p.Spec.HostKeyIDs))
	for _, id := range p.Spec.HostKeyIDs {
		name, _ := rev.HostKeyName(id)
		if name == "" {
			name = id
		}
		hostKeys = append(hostKeys, name)
	}

	rlName := ""
	if p.Spec.RateLimitID != "" {
		name, _ := rev.RateLimitName(p.Spec.RateLimitID)
		if name == "" {
			name = p.Spec.RateLimitID
		}
		rlName = name
	}

	return PolicyDTO{
		APIVersion: APIVersion,
		Kind:       "Policy",
		Metadata:   metaToWire(p.Meta),
		Spec: PolicySpec{
			Models:            models,
			HostKeys:          hostKeys,
			RateLimit:         rlName,
			KeySelection:      string(p.Spec.KeySelection),
			SkipDefaultLimits: p.Spec.SkipDefaultLimits,
			IncludeDeprecated: p.Spec.IncludeDeprecated,
			Enabled:           p.Spec.Enabled,
		},
	}
}

// ---------------------------------------------------------------------------
// RateLimit
// ---------------------------------------------------------------------------

// ToRateLimit converts a RateLimitDTO to a domain RateLimit. Resolves
// owner.id from a host *name* to its id when Owner.Kind=host (the wire
// form uses names for human readability).
func ToRateLimit(d RateLimitDTO, idx Resolver) (*ratelimit.RateLimit, error) {
	rules := make([]ratelimit.Rule, 0, len(d.Spec.Rules))
	for i, r := range d.Spec.Rules {
		w, err := parseDuration(r.Window)
		if err != nil {
			return nil, fmt.Errorf("ratelimit %q: rules[%d].window: %w", d.Metadata.Name, i, err)
		}
		rules = append(rules, ratelimit.Rule{
			Meter:    ratelimit.Meter(r.Meter),
			Amount:   r.Amount,
			Window:   w,
			Strategy: ratelimit.Strategy(r.Strategy),
		})
	}
	m := d.Metadata.toMeta()
	if m.Owner.Kind == meta.OwnerHost && m.Owner.ID != "" {
		if hid, ok := idx.HostID(m.Owner.ID); ok {
			m.Owner.ID = hid
		}
	}
	return &ratelimit.RateLimit{
		Meta: m,
		Spec: ratelimit.Spec{
			Rules:   rules,
			Enabled: d.Spec.Enabled,
		},
	}, nil
}

func FromRateLimit(rl *ratelimit.RateLimit, _ ReverseResolver) RateLimitDTO {
	rules := make([]RateLimitRule, 0, len(rl.Spec.Rules))
	for _, r := range rl.Spec.Rules {
		rules = append(rules, RateLimitRule{
			Meter:    string(r.Meter),
			Amount:   r.Amount,
			Window:   r.Window.String(),
			Strategy: string(r.Strategy),
		})
	}
	return RateLimitDTO{
		APIVersion: APIVersion,
		Kind:       "RateLimit",
		Metadata:   metaToWire(rl.Meta),
		Spec: RateLimitSpec{
			Rules:   rules,
			Enabled: rl.Spec.Enabled,
		},
	}
}

// ---------------------------------------------------------------------------
// Pricing
// ---------------------------------------------------------------------------

// ToPricing resolves the owner host name → id and target model names → ids.
func ToPricing(d PricingDTO, idx Resolver) (*pricing.Pricing, error) {
	m := d.Metadata.toMeta()
	if m.Owner.Kind == meta.OwnerHost && m.Owner.ID != "" {
		if hid, ok := idx.HostID(m.Owner.ID); ok {
			m.Owner.ID = hid
		}
	}

	modelIDs := make([]string, 0, len(d.Spec.TargetModels))
	for _, name := range d.Spec.TargetModels {
		id, ok := idx.ModelID(name)
		if !ok {
			return nil, fmt.Errorf("pricing %q: targetModels: model %q not found", d.Metadata.Name, name)
		}
		modelIDs = append(modelIDs, id)
	}

	rates := make([]pricing.Rate, 0, len(d.Spec.Rates))
	for _, r := range d.Spec.Rates {
		rates = append(rates, pricing.Rate{
			Meter:       pricing.Meter(r.Meter),
			Unit:        pricing.Unit(r.Unit),
			Amount:      r.Amount,
			AboveTokens: r.AboveTokens,
		})
	}

	return &pricing.Pricing{
		Meta: m,
		Spec: pricing.Spec{
			Currency:       d.Spec.Currency,
			TargetModelIDs: modelIDs,
			Rates:          rates,
			Enabled:        d.Spec.Enabled,
		},
	}, nil
}

func FromPricing(p *pricing.Pricing, rev ReverseResolver) PricingDTO {
	wm := metaToWire(p.Meta)
	if p.Meta.Owner.Kind == meta.OwnerHost && p.Meta.Owner.ID != "" {
		if hname, ok := rev.HostName(p.Meta.Owner.ID); ok {
			wm.Owner.Name = hname
		}
	}

	models := make([]string, 0, len(p.Spec.TargetModelIDs))
	for _, id := range p.Spec.TargetModelIDs {
		name, _ := rev.ModelName(id)
		if name == "" {
			name = id
		}
		models = append(models, name)
	}

	rates := make([]PricingRateDTO, 0, len(p.Spec.Rates))
	for _, r := range p.Spec.Rates {
		rates = append(rates, PricingRateDTO{
			Meter:       string(r.Meter),
			Unit:        string(r.Unit),
			Amount:      r.Amount,
			AboveTokens: r.AboveTokens,
		})
	}

	return PricingDTO{
		APIVersion: APIVersion,
		Kind:       "Pricing",
		Metadata:   wm,
		Spec: PricingSpec{
			Currency:     p.Spec.Currency,
			TargetModels: models,
			Rates:        rates,
			Enabled:      p.Spec.Enabled,
		},
	}
}

// parseDuration handles a Window field that may be either a human-readable
// string ("30s", "1m") or an int64 nanosecond count.
func parseDuration(v interface{}) (time.Duration, error) {
	if v == nil {
		return 0, fmt.Errorf("window is required")
	}
	switch val := v.(type) {
	case string:
		return time.ParseDuration(val)
	case int:
		return time.Duration(val), nil
	case int64:
		return time.Duration(val), nil
	case float64:
		return time.Duration(int64(val)), nil
	default:
		return 0, fmt.Errorf("unsupported window type %T", v)
	}
}

// ---------------------------------------------------------------------------
// RelayKey
// ---------------------------------------------------------------------------

func ToRelayKey(d RelayKeyDTO, idx Resolver) (*relaykey.RelayKey, error) {
	policyID, ok := idx.PolicyID(d.Spec.Policy)
	if !ok {
		return nil, fmt.Errorf("relaykey %q: policy %q not found", d.Metadata.Name, d.Spec.Policy)
	}

	var revokedAt *time.Time
	if d.Spec.RevokedAt != nil {
		t, err := time.Parse(time.RFC3339, *d.Spec.RevokedAt)
		if err != nil {
			return nil, fmt.Errorf("relaykey %q: revokedAt: %w", d.Metadata.Name, err)
		}
		revokedAt = &t
	}

	return &relaykey.RelayKey{
		Meta: d.Metadata.toMeta(),
		Spec: relaykey.Spec{
			PolicyID:           policyID,
			KeyHash:            d.Spec.KeyHash,
			Prefix:             d.Spec.Prefix,
			RevokedAt:          revokedAt,
			Enabled:            d.Spec.Enabled,
			PassthroughAllowed: d.Spec.PassthroughAllowed,
		},
	}, nil
}

func FromRelayKey(k *relaykey.RelayKey, rev ReverseResolver) RelayKeyDTO {
	policyName, _ := rev.PolicyName(k.Spec.PolicyID)
	if policyName == "" {
		policyName = k.Spec.PolicyID
	}

	var revokedAt *string
	if k.Spec.RevokedAt != nil {
		s := k.Spec.RevokedAt.Format(time.RFC3339)
		revokedAt = &s
	}

	return RelayKeyDTO{
		APIVersion: APIVersion,
		Kind:       "RelayKey",
		Metadata:   metaToWire(k.Meta),
		Spec: RelayKeySpec{
			Policy:             policyName,
			KeyHash:            k.Spec.KeyHash,
			Prefix:             k.Spec.Prefix,
			RevokedAt:          revokedAt,
			Enabled:            k.Spec.Enabled,
			PassthroughAllowed: k.Spec.PassthroughAllowed,
		},
	}
}
