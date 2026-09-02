package manifest

import (
	"fmt"
	"strings"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
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

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

func ToProvider(d ProviderDTO, _ Resolver) (*provider.Provider, error) {
	p := &provider.Provider{
		Meta: d.Metadata.toMeta(),
		Spec: provider.Spec{
			Enabled:       d.Spec.Enabled,
			HomepageURL:   d.Spec.HomepageURL,
			DocsURL:       d.Spec.DocsURL,
			StatusPageURL: d.Spec.StatusPageURL,
			Icon:          d.Spec.Icon,
		},
	}
	// Default owner.kind to "system" when the wire form left it empty
	// (catalog-supplied providers are system-owned by convention; BYO
	// providers must declare kind: user explicitly).
	if p.Meta.Owner.Kind == "" {
		p.Meta.Owner.Kind = meta.OwnerSystem
	}
	return p, nil
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
	h := &host.Host{
		Meta: d.Metadata.toMeta(),
		Spec: host.Spec{
			BaseURL:           d.Spec.BaseURL,
			Path:              d.Spec.Path,
			Backend:           d.Spec.Backend,
			Policies:          policies,
			DefaultPolicy:     defaultPolicy,
			NoAuth:            d.Spec.NoAuth,
			PricingStrategies: d.Spec.PricingStrategies,
			Enabled:           d.Spec.Enabled,
			HomepageURL:       d.Spec.HomepageURL,
			DocsURL:           d.Spec.DocsURL,
			ConsoleURL:        d.Spec.ConsoleURL,
			StatusPageURL:     d.Spec.StatusPageURL,
			Icon:              d.Spec.Icon,
		},
	}
	// Default owner.kind to "system" when wire form left it empty.
	if h.Meta.Owner.Kind == "" {
		h.Meta.Owner.Kind = meta.OwnerSystem
	}
	return h, nil
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
			BaseURL:           h.Spec.BaseURL,
			Path:              h.Spec.Path,
			Backend:           h.Spec.Backend,
			Policies:          policies,
			DefaultPolicy:     defaultPolicy,
			NoAuth:            h.Spec.NoAuth,
			PricingStrategies: h.Spec.PricingStrategies,
			Enabled:           h.Spec.Enabled,
			HomepageURL:       h.Spec.HomepageURL,
			DocsURL:           h.Spec.DocsURL,
			ConsoleURL:        h.Spec.ConsoleURL,
			StatusPageURL:     h.Spec.StatusPageURL,
			Icon:              h.Spec.Icon,
		},
	}
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

// ToModel resolves the model's owning provider name to an id.
//
// Provider owner: the wire form stores the provider *name* in Metadata.Owner.ID
// when coming from YAML. Callers who need name→id resolution for the owner
// should do so before this call, or supply the provider id directly.
func ToModel(d ModelDTO, idx Resolver) (*model.Model, error) {
	m := &model.Model{
		Meta: d.Metadata.toMeta(),
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
		Tags:                 d.Spec.Tags,
		Documentation:        d.Spec.Documentation,
		License:              d.Spec.License,
		ProviderModelPageURL: d.Spec.ProviderModelPageURL,
		Enabled:              d.Spec.Enabled,
		Snapshots:            d.Spec.Snapshots,
		Pointer:              d.Spec.Pointer,
		Aliases:              d.Spec.Aliases,
	}
	return m, nil
}

func FromModel(m *model.Model, rev ReverseResolver) ModelDTO {
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
			Tags:                 m.Spec.Tags,
			Documentation:        m.Spec.Documentation,
			License:              m.Spec.License,
			ProviderModelPageURL: m.Spec.ProviderModelPageURL,
			Enabled:              m.Spec.Enabled,
			Snapshots:            m.Spec.Snapshots,
			Pointer:              m.Spec.Pointer,
			Aliases:              m.Spec.Aliases,
		},
	}
}

// ---------------------------------------------------------------------------
// HostKey
// ---------------------------------------------------------------------------

// ToHostKey resolves Spec.HostID and Spec.PolicyID (name → id).
func ToHostKey(d HostKeyDTO, idx Resolver) (*hostkey.HostKey, error) {
	m := d.Metadata.toMeta()
	if m.Owner.Kind == "" {
		m.Owner.Kind = meta.OwnerSystem
	}
	resolveScopeOwner(&m.Owner, idx)
	hostID := d.Spec.HostID
	if hostID != "" {
		if id, ok := idx.HostID(hostID); ok {
			hostID = id
		}
	}
	policyID := d.Spec.PolicyID
	if policyID != "" {
		if id, ok := idx.PolicyID(policyID); ok {
			policyID = id
		}
	}
	return &hostkey.HostKey{
		Meta: m,
		Spec: hostkey.Spec{
			HostID:   hostID,
			PolicyID: policyID,
			ValueFrom: hostkey.ValueFrom{
				Kind:     hostkey.ValueKind(d.Spec.ValueFrom.Kind),
				Env:      d.Spec.ValueFrom.Env,
				Provider: d.Spec.ValueFrom.Provider,
			},
			DefaultTier:     d.Spec.DefaultTier,
			PricingStrategy: d.Spec.PricingStrategy,
			Enabled:         d.Spec.Enabled,
			Value:           d.Spec.Value,
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
	policyID := k.Spec.PolicyID
	if policyID != "" {
		if pname, ok := rev.PolicyName(policyID); ok {
			policyID = pname
		}
	}
	return HostKeyDTO{
		APIVersion: APIVersion,
		Kind:       "HostKey",
		Metadata:   wm,
		Spec: HostKeySpec{
			HostID:   hostID,
			PolicyID: policyID,
			ValueFrom: HostKeyValueFrom{
				Kind:     string(k.Spec.ValueFrom.Kind),
				Env:      k.Spec.ValueFrom.Env,
				Provider: k.Spec.ValueFrom.Provider,
			},
			DefaultTier:     k.Spec.DefaultTier,
			PricingStrategy: k.Spec.PricingStrategy,
			Enabled:         k.Spec.Enabled,
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

	rlBindings := make([]policy.RLBinding, 0, len(d.Spec.RLBindings))
	for i, b := range d.Spec.RLBindings {
		if b.RateLimit == "" {
			return nil, fmt.Errorf("policy %q: rlBindings[%d].rateLimit is required", d.Metadata.Name, i)
		}
		id, ok := idx.RateLimitID(b.RateLimit)
		if !ok {
			return nil, fmt.Errorf("policy %q: rlBindings[%d] rateLimit %q not found",
				d.Metadata.Name, i, b.RateLimit)
		}
		rlBindings = append(rlBindings, policy.RLBinding{
			Models:      append([]string{}, b.Models...),
			RateLimitID: id,
		})
	}

	m := d.Metadata.toMeta()
	if m.Owner.Kind == meta.OwnerHost && m.Owner.ID != "" {
		if hid, ok := idx.HostID(m.Owner.ID); ok {
			m.Owner.ID = hid
		}
	}
	resolveScopeOwner(&m.Owner, idx)
	return &policy.Policy{
		Meta: m,
		Spec: policy.Spec{
			Models:                models,
			HostKeyIDs:            hostKeyIDs,
			RateLimitID:           rateLimitID,
			RLBindings:            rlBindings,
			KeySelection:          policy.KeySelection(d.Spec.KeySelection),
			SkipDefaultLimits:     d.Spec.SkipDefaultLimits,
			IncludeDeprecated:     d.Spec.IncludeDeprecated,
			Enabled:               d.Spec.Enabled,
			PayloadLoggingEnabled: d.Spec.PayloadLoggingEnabled,
		},
	}, nil
}

func FromPolicy(p *policy.Policy, rev ReverseResolver) PolicyDTO {
	// Spec.Models is already in wire form (ref strings). Spec.ModelIDs is the
	// legacy literal-ID grant; emit those rows as model refs, not bare model
	// slugs, because a bare modelref token means "provider".
	models := make([]string, 0, len(p.Spec.Models)+len(p.Spec.ModelIDs))
	models = append(models, p.Spec.Models...)
	for _, id := range p.Spec.ModelIDs {
		models = append(models, legacyModelRef(id, rev))
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

	bindings := make([]RLBindingDTO, 0, len(p.Spec.RLBindings))
	for _, b := range p.Spec.RLBindings {
		rl := b.RateLimitID
		if name, ok := rev.RateLimitName(rl); ok {
			rl = name
		}
		bindings = append(bindings, RLBindingDTO{
			Models:    append([]string{}, b.Models...),
			RateLimit: rl,
		})
	}

	return PolicyDTO{
		APIVersion: APIVersion,
		Kind:       "Policy",
		Metadata:   metaToWire(p.Meta),
		Spec: PolicySpec{
			Models:                models,
			HostKeys:              hostKeys,
			RateLimit:             rlName,
			RLBindings:            bindings,
			KeySelection:          string(p.Spec.KeySelection),
			SkipDefaultLimits:     p.Spec.SkipDefaultLimits,
			IncludeDeprecated:     p.Spec.IncludeDeprecated,
			Enabled:               p.Spec.Enabled,
			PayloadLoggingEnabled: p.Spec.PayloadLoggingEnabled,
		},
	}
}

type modelProviderIDResolver interface {
	ModelProviderID(modelID string) (string, bool)
}

// legacyModelRef renders a legacy ModelIDs grant as a modelref. A bare token
// means "provider" in the DSL, so a bare model slug would re-import as the
// wrong grant; emit the provider-qualified "provider/model" form when the
// resolver can supply the model's provider, else fall back to the name.
func legacyModelRef(id string, rev ReverseResolver) string {
	name, _ := rev.ModelName(id)
	if name == "" {
		return id
	}
	if strings.Contains(name, "/") {
		return name
	}
	if r, ok := rev.(modelProviderIDResolver); ok {
		if providerID, ok := r.ModelProviderID(id); ok {
			if provider, ok := rev.ProviderName(providerID); ok && provider != "" {
				return provider + "/" + name
			}
		}
	}
	return name
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
			Window:   ratelimit.Window(w),
			Strategy: ratelimit.Strategy(r.Strategy),
		})
	}
	m := d.Metadata.toMeta()
	if m.Owner.Kind == meta.OwnerHost && m.Owner.ID != "" {
		if hid, ok := idx.HostID(m.Owner.ID); ok {
			m.Owner.ID = hid
		}
	}
	resolveScopeOwner(&m.Owner, idx)
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
			Window:   r.Window.Duration().String(),
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
// HostBinding
// ---------------------------------------------------------------------------

// ToHostBinding resolves model, host, and (optional) pricing names to ids.
func ToHostBinding(d HostBindingDTO, idx Resolver) (*binding.Binding, error) {
	modelID, ok := idx.ModelID(d.Spec.Model)
	if !ok {
		return nil, fmt.Errorf("hostbinding %q: model %q not found", d.Metadata.Name, d.Spec.Model)
	}
	hostID, ok := idx.HostID(d.Spec.Host)
	if !ok {
		return nil, fmt.Errorf("hostbinding %q: host %q not found", d.Metadata.Name, d.Spec.Host)
	}
	var pricingID string
	if d.Spec.Pricing != "" {
		pid, ok := idx.PricingID(d.Spec.Pricing)
		if !ok {
			return nil, fmt.Errorf("hostbinding %q: pricing %q not found", d.Metadata.Name, d.Spec.Pricing)
		}
		pricingID = pid
	}
	m := d.Metadata.toMeta()
	if m.Owner.Kind == "" {
		m.Owner.Kind = meta.OwnerSystem
	}
	return &binding.Binding{
		Meta: m,
		Spec: binding.Spec{
			ModelID:      modelID,
			HostID:       hostID,
			Adapter:      adapters.Name(d.Spec.Adapter),
			UpstreamName: d.Spec.UpstreamName,
			PricingID:    pricingID,
			Enabled:      d.Spec.Enabled,
			Snapshots:    d.Spec.Snapshots,
		},
	}, nil
}

func FromHostBinding(b *binding.Binding, rev ReverseResolver) HostBindingDTO {
	modelName, _ := rev.ModelName(b.Spec.ModelID)
	if modelName == "" {
		modelName = b.Spec.ModelID
	}
	hostName, _ := rev.HostName(b.Spec.HostID)
	if hostName == "" {
		hostName = b.Spec.HostID
	}
	pricingName := ""
	if b.Spec.PricingID != "" {
		n, _ := rev.PricingName(b.Spec.PricingID)
		if n == "" {
			n = b.Spec.PricingID
		}
		pricingName = n
	}
	return HostBindingDTO{
		APIVersion: APIVersion,
		Kind:       "HostBinding",
		Metadata:   metaToWire(b.Meta),
		Spec: HostBindingSpec{
			Model:        modelName,
			Host:         hostName,
			Adapter:      string(b.Spec.Adapter),
			UpstreamName: b.Spec.UpstreamName,
			Pricing:      pricingName,
			Enabled:      b.Spec.Enabled,
			Snapshots:    b.Spec.Snapshots,
		},
	}
}

// ---------------------------------------------------------------------------
// Key
// ---------------------------------------------------------------------------

func ToKey(d KeyDTO, idx Resolver) (*key.Key, error) {
	policyID, ok := idx.PolicyID(d.Spec.Policy)
	if !ok {
		return nil, fmt.Errorf("key %q: policy %q not found", d.Metadata.Name, d.Spec.Policy)
	}

	principal, err := toPrincipal(d.Metadata.Name, d.Spec.Principal, idx)
	if err != nil {
		return nil, err
	}

	expiresAt, err := parseOptionalTime(d.Metadata.Name, "expiresAt", d.Spec.ExpiresAt)
	if err != nil {
		return nil, err
	}
	revokedAt, err := parseOptionalTime(d.Metadata.Name, "revokedAt", d.Spec.RevokedAt)
	if err != nil {
		return nil, err
	}

	m := d.Metadata.toMeta()
	resolveScopeOwner(&m.Owner, idx)
	return &key.Key{
		Meta: m,
		Spec: key.Spec{
			Principal:             principal,
			PolicyID:              policyID,
			KeyHash:               d.Spec.KeyHash,
			Prefix:                d.Spec.Prefix,
			ExpiresAt:             expiresAt,
			RevokedAt:             revokedAt,
			Enabled:               d.Spec.Enabled,
			PassthroughAllowed:    d.Spec.PassthroughAllowed,
			PayloadLoggingEnabled: d.Spec.PayloadLoggingEnabled,
		},
	}, nil
}

// toPrincipal resolves the wire principal name to an id: a service account
// slug or a username, per kind.
func toPrincipal(keyName string, p PrincipalDTO, idx Resolver) (key.Principal, error) {
	switch key.PrincipalKind(p.Kind) {
	case key.PrincipalServiceAccount:
		id, ok := idx.ServiceAccountID(p.Name)
		if !ok {
			return key.Principal{}, fmt.Errorf("key %q: service account %q not found", keyName, p.Name)
		}
		return key.Principal{Kind: key.PrincipalServiceAccount, ID: id}, nil
	case key.PrincipalUser:
		id, ok := idx.UserID(p.Name)
		if !ok {
			return key.Principal{}, fmt.Errorf("key %q: user %q not found", keyName, p.Name)
		}
		return key.Principal{Kind: key.PrincipalUser, ID: id}, nil
	default:
		return key.Principal{}, fmt.Errorf("key %q: principal.kind must be serviceaccount or user, got %q", keyName, p.Kind)
	}
}

func parseOptionalTime(name, field string, raw *string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, *raw)
	if err != nil {
		return nil, fmt.Errorf("key %q: %s: %w", name, field, err)
	}
	return &t, nil
}

func FromKey(k *key.Key, rev ReverseResolver) KeyDTO {
	policyName, _ := rev.PolicyName(k.Spec.PolicyID)
	if policyName == "" {
		policyName = k.Spec.PolicyID
	}

	principal := PrincipalDTO{Kind: string(k.Spec.Principal.Kind), Name: k.Spec.Principal.ID}
	switch k.Spec.Principal.Kind {
	case key.PrincipalServiceAccount:
		if n, ok := rev.ServiceAccountName(k.Spec.Principal.ID); ok {
			principal.Name = n
		}
	case key.PrincipalUser:
		if n, ok := rev.Username(k.Spec.Principal.ID); ok {
			principal.Name = n
		}
	}

	return KeyDTO{
		APIVersion: APIVersion,
		Kind:       "Key",
		Metadata:   metaToWire(k.Meta),
		Spec: KeySpec{
			Principal:             principal,
			Policy:                policyName,
			KeyHash:               k.Spec.KeyHash,
			Prefix:                k.Spec.Prefix,
			ExpiresAt:             formatOptionalTime(k.Spec.ExpiresAt),
			RevokedAt:             formatOptionalTime(k.Spec.RevokedAt),
			Enabled:               k.Spec.Enabled,
			PassthroughAllowed:    k.Spec.PassthroughAllowed,
			PayloadLoggingEnabled: k.Spec.PayloadLoggingEnabled,
		},
	}
}

func formatOptionalTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(time.RFC3339)
	return &s
}

// ---------------------------------------------------------------------------
// Team / Project
// ---------------------------------------------------------------------------

// resolveScopeOwner rewrites a team- or project-kind owner from the wire
// name to its id. Rows in other scopes are untouched.
func resolveScopeOwner(o *meta.Owner, idx Resolver) {
	if o.ID == "" {
		return
	}
	switch o.Kind {
	case meta.OwnerTeam:
		if id, ok := idx.TeamID(o.ID); ok {
			o.ID = id
		}
	case meta.OwnerProject:
		if id, ok := idx.ProjectID(o.ID); ok {
			o.ID = id
		}
	}
}

func toBudget(b *BudgetDTO) *team.Budget {
	if b == nil {
		return nil
	}
	return &team.Budget{Amount: b.Amount, Period: b.Period, OnExceed: b.OnExceed}
}

func fromBudget(b *team.Budget) *BudgetDTO {
	if b == nil {
		return nil
	}
	return &BudgetDTO{Amount: b.Amount, Period: b.Period, OnExceed: b.OnExceed}
}

func ToTeam(d TeamDTO, _ Resolver) (*team.Team, error) {
	m := d.Metadata.toMeta()
	// Seeded teams are system-owned by convention; API-created ones declare
	// kind: user explicitly.
	if m.Owner.Kind == "" {
		m.Owner.Kind = meta.OwnerSystem
	}
	return &team.Team{
		Meta: m,
		Spec: team.Spec{
			Enabled: d.Spec.Enabled,
			Budget:  toBudget(d.Spec.Budget),
		},
	}, nil
}

func FromTeam(t *team.Team, _ ReverseResolver) TeamDTO {
	return TeamDTO{
		APIVersion: APIVersion,
		Kind:       "Team",
		Metadata:   metaToWire(t.Meta),
		Spec: TeamSpec{
			Enabled: t.Spec.Enabled,
			Budget:  fromBudget(t.Spec.Budget),
		},
	}
}

// ToProject resolves the owning team name → id. Owner mirrors spec.team,
// so it is re-derived rather than read from the wire form.
func ToProject(d ProjectDTO, idx Resolver) (*project.Project, error) {
	teamID, ok := idx.TeamID(d.Spec.Team)
	if !ok {
		return nil, fmt.Errorf("project %q: team %q not found", d.Metadata.Name, d.Spec.Team)
	}
	p := &project.Project{
		Meta: d.Metadata.toMeta(),
		Spec: project.Spec{
			TeamID:  teamID,
			Enabled: d.Spec.Enabled,
			Budget:  toBudget(d.Spec.Budget),
		},
	}
	p.StampOwner()
	return p, nil
}

func FromProject(p *project.Project, rev ReverseResolver) ProjectDTO {
	teamName, _ := rev.TeamName(p.Spec.TeamID)
	if teamName == "" {
		teamName = p.Spec.TeamID
	}
	wm := metaToWire(p.Meta)
	wm.Owner.Name = teamName
	return ProjectDTO{
		APIVersion: APIVersion,
		Kind:       "Project",
		Metadata:   wm,
		Spec: ProjectSpec{
			Team:    teamName,
			Enabled: p.Spec.Enabled,
			Budget:  fromBudget(p.Spec.Budget),
		},
	}
}

// ---------------------------------------------------------------------------
// ServiceAccount / Group
// ---------------------------------------------------------------------------

// ToServiceAccount resolves the owning project name → id and the optional
// policy override. Owner mirrors spec.project, so it is re-derived rather
// than read from the wire form.
func ToServiceAccount(d ServiceAccountDTO, idx Resolver) (*serviceaccount.ServiceAccount, error) {
	projectID, ok := idx.ProjectID(d.Spec.Project)
	if !ok {
		return nil, fmt.Errorf("serviceaccount %q: project %q not found", d.Metadata.Name, d.Spec.Project)
	}
	var policyID string
	if d.Spec.Policy != "" {
		policyID, ok = idx.PolicyID(d.Spec.Policy)
		if !ok {
			return nil, fmt.Errorf("serviceaccount %q: policy %q not found", d.Metadata.Name, d.Spec.Policy)
		}
	}
	sa := &serviceaccount.ServiceAccount{
		Meta: d.Metadata.toMeta(),
		Spec: serviceaccount.Spec{
			ProjectID: projectID,
			PolicyID:  policyID,
			Enabled:   d.Spec.Enabled,
		},
	}
	sa.StampOwner()
	return sa, nil
}

func FromServiceAccount(sa *serviceaccount.ServiceAccount, rev ReverseResolver) ServiceAccountDTO {
	projectName, _ := rev.ProjectName(sa.Spec.ProjectID)
	if projectName == "" {
		projectName = sa.Spec.ProjectID
	}
	policyName := ""
	if sa.Spec.PolicyID != "" {
		policyName, _ = rev.PolicyName(sa.Spec.PolicyID)
		if policyName == "" {
			policyName = sa.Spec.PolicyID
		}
	}
	wm := metaToWire(sa.Meta)
	wm.Owner.Name = projectName
	return ServiceAccountDTO{
		APIVersion: APIVersion,
		Kind:       "ServiceAccount",
		Metadata:   wm,
		Spec: ServiceAccountSpec{
			Project: projectName,
			Policy:  policyName,
			Enabled: sa.Spec.Enabled,
		},
	}
}

// ToGroup resolves member usernames → user ids.
func ToGroup(d GroupDTO, idx Resolver) (*group.Group, error) {
	m := d.Metadata.toMeta()
	// Seeded groups are system-owned by convention; API-created ones declare
	// kind: user explicitly.
	if m.Owner.Kind == "" {
		m.Owner.Kind = meta.OwnerSystem
	}
	var memberIDs []string
	for _, username := range d.Spec.Members {
		id, ok := idx.UserID(username)
		if !ok {
			return nil, fmt.Errorf("group %q: user %q not found", d.Metadata.Name, username)
		}
		memberIDs = append(memberIDs, id)
	}
	return &group.Group{
		Meta: m,
		Spec: group.Spec{MemberIDs: memberIDs, Enabled: d.Spec.Enabled},
	}, nil
}

func FromGroup(g *group.Group, rev ReverseResolver) GroupDTO {
	var members []string
	for _, id := range g.Spec.MemberIDs {
		name, ok := rev.Username(id)
		if !ok {
			name = id
		}
		members = append(members, name)
	}
	return GroupDTO{
		APIVersion: APIVersion,
		Kind:       "Group",
		Metadata:   metaToWire(g.Meta),
		Spec:       GroupSpec{Members: members, Enabled: g.Spec.Enabled},
	}
}

// ---------------------------------------------------------------------------
// Role / RoleBinding / PolicyBinding
// ---------------------------------------------------------------------------

func ToRole(d RoleDTO, _ Resolver) (*role.Role, error) {
	m := d.Metadata.toMeta()
	// Seeded roles are system-owned by convention; API-created ones declare
	// kind: user explicitly.
	if m.Owner.Kind == "" {
		m.Owner.Kind = meta.OwnerSystem
	}
	rules := make([]role.Rule, 0, len(d.Spec.Rules))
	for _, r := range d.Spec.Rules {
		rules = append(rules, role.Rule{Kinds: r.Kinds, Verbs: r.Verbs})
	}
	return &role.Role{
		Meta: m,
		Spec: role.Spec{Rules: rules, Enabled: d.Spec.Enabled},
	}, nil
}

func FromRole(r *role.Role, _ ReverseResolver) RoleDTO {
	rules := make([]RoleRuleDTO, 0, len(r.Spec.Rules))
	for _, rule := range r.Spec.Rules {
		rules = append(rules, RoleRuleDTO{Kinds: rule.Kinds, Verbs: rule.Verbs})
	}
	return RoleDTO{
		APIVersion: APIVersion,
		Kind:       "Role",
		Metadata:   metaToWire(r.Meta),
		Spec:       RoleSpec{Rules: rules, Enabled: r.Spec.Enabled},
	}
}

// toSubjects resolves named subjects to the form the snapshot indexes:
// users and service accounts by id, groups by name.
func toSubjects(name string, subjects []SubjectDTO, idx Resolver) ([]rolebinding.Subject, error) {
	out := make([]rolebinding.Subject, 0, len(subjects))
	for _, sub := range subjects {
		switch rolebinding.SubjectKind(sub.Kind) {
		case rolebinding.SubjectGroup:
			out = append(out, rolebinding.Subject{Kind: rolebinding.SubjectGroup, Name: sub.Name})
		case rolebinding.SubjectUser:
			id, ok := idx.UserID(sub.Name)
			if !ok {
				return nil, fmt.Errorf("binding %q: user %q not found", name, sub.Name)
			}
			out = append(out, rolebinding.Subject{Kind: rolebinding.SubjectUser, ID: id})
		case rolebinding.SubjectServiceAccount:
			id, ok := idx.ServiceAccountID(sub.Name)
			if !ok {
				return nil, fmt.Errorf("binding %q: service account %q not found", name, sub.Name)
			}
			out = append(out, rolebinding.Subject{Kind: rolebinding.SubjectServiceAccount, ID: id})
		default:
			return nil, fmt.Errorf("binding %q: subject.kind must be user, group, or serviceaccount, got %q", name, sub.Kind)
		}
	}
	return out, nil
}

func fromSubjects(subjects []rolebinding.Subject, rev ReverseResolver) []SubjectDTO {
	out := make([]SubjectDTO, 0, len(subjects))
	for _, sub := range subjects {
		wire := SubjectDTO{Kind: string(sub.Kind), Name: sub.Name}
		switch sub.Kind {
		case rolebinding.SubjectUser:
			if n, ok := rev.Username(sub.ID); ok {
				wire.Name = n
			} else {
				wire.Name = sub.ID
			}
		case rolebinding.SubjectServiceAccount:
			if n, ok := rev.ServiceAccountName(sub.ID); ok {
				wire.Name = n
			} else {
				wire.Name = sub.ID
			}
		}
		out = append(out, wire)
	}
	return out
}

// ToRoleBinding resolves the role name, the scope target, and the subjects.
// Owner mirrors spec.scope, so it is re-derived rather than read from the
// wire form.
func ToRoleBinding(d RoleBindingDTO, idx Resolver) (*rolebinding.RoleBinding, error) {
	roleID, ok := idx.RoleID(d.Spec.Role)
	if !ok {
		return nil, fmt.Errorf("rolebinding %q: role %q not found", d.Metadata.Name, d.Spec.Role)
	}
	scope := meta.Owner{Kind: d.Spec.Scope.Kind, ID: d.Spec.Scope.ref()}
	switch scope.Kind {
	case meta.OwnerTeam:
		id, ok := idx.TeamID(scope.ID)
		if !ok {
			return nil, fmt.Errorf("rolebinding %q: team %q not found", d.Metadata.Name, scope.ID)
		}
		scope.ID = id
	case meta.OwnerProject:
		id, ok := idx.ProjectID(scope.ID)
		if !ok {
			return nil, fmt.Errorf("rolebinding %q: project %q not found", d.Metadata.Name, scope.ID)
		}
		scope.ID = id
	}
	subjects, err := toSubjects(d.Metadata.Name, d.Spec.Subjects, idx)
	if err != nil {
		return nil, err
	}
	b := &rolebinding.RoleBinding{
		Meta: d.Metadata.toMeta(),
		Spec: rolebinding.Spec{
			RoleID:   roleID,
			Scope:    scope,
			Subjects: subjects,
			Enabled:  d.Spec.Enabled,
		},
	}
	b.StampOwner()
	return b, nil
}

func FromRoleBinding(b *rolebinding.RoleBinding, rev ReverseResolver) RoleBindingDTO {
	roleName, _ := rev.RoleName(b.Spec.RoleID)
	if roleName == "" {
		roleName = b.Spec.RoleID
	}
	scope := WireOwner{Kind: b.Spec.Scope.Kind, Name: b.Spec.Scope.ID}
	switch b.Spec.Scope.Kind {
	case meta.OwnerTeam:
		if name, ok := rev.TeamName(b.Spec.Scope.ID); ok {
			scope.Name = name
		}
	case meta.OwnerProject:
		if name, ok := rev.ProjectName(b.Spec.Scope.ID); ok {
			scope.Name = name
		}
	}
	wm := metaToWire(b.Meta)
	wm.Owner.Name = scope.Name
	return RoleBindingDTO{
		APIVersion: APIVersion,
		Kind:       "RoleBinding",
		Metadata:   wm,
		Spec: RoleBindingSpec{
			Role:     roleName,
			Scope:    scope,
			Subjects: fromSubjects(b.Spec.Subjects, rev),
			Enabled:  b.Spec.Enabled,
		},
	}
}

// ToPolicyBinding resolves the project, the policy, and the subjects. Owner
// mirrors spec.project.
func ToPolicyBinding(d PolicyBindingDTO, idx Resolver) (*policybinding.PolicyBinding, error) {
	projectID, ok := idx.ProjectID(d.Spec.Project)
	if !ok {
		return nil, fmt.Errorf("policybinding %q: project %q not found", d.Metadata.Name, d.Spec.Project)
	}
	policyID, ok := idx.PolicyID(d.Spec.Policy)
	if !ok {
		return nil, fmt.Errorf("policybinding %q: policy %q not found", d.Metadata.Name, d.Spec.Policy)
	}
	subjects, err := toSubjects(d.Metadata.Name, d.Spec.Subjects, idx)
	if err != nil {
		return nil, err
	}
	priority := d.Spec.Priority
	if priority == 0 {
		// The store and the control API both stamp this default, so a
		// document that omits priority must translate to the same row or
		// apply reports an update on every run.
		priority = policybinding.DefaultPriority
	}
	b := &policybinding.PolicyBinding{
		Meta: d.Metadata.toMeta(),
		Spec: policybinding.Spec{
			ProjectID: projectID,
			PolicyID:  policyID,
			Priority:  priority,
			Subjects:  subjects,
			Enabled:   d.Spec.Enabled,
		},
	}
	b.StampOwner()
	return b, nil
}

func FromPolicyBinding(b *policybinding.PolicyBinding, rev ReverseResolver) PolicyBindingDTO {
	projectName, _ := rev.ProjectName(b.Spec.ProjectID)
	if projectName == "" {
		projectName = b.Spec.ProjectID
	}
	policyName, _ := rev.PolicyName(b.Spec.PolicyID)
	if policyName == "" {
		policyName = b.Spec.PolicyID
	}
	wm := metaToWire(b.Meta)
	wm.Owner.Name = projectName
	return PolicyBindingDTO{
		APIVersion: APIVersion,
		Kind:       "PolicyBinding",
		Metadata:   wm,
		Spec: PolicyBindingSpec{
			Project:  projectName,
			Policy:   policyName,
			Priority: b.EffectivePriority(),
			Subjects: fromSubjects(b.Spec.Subjects, rev),
			Enabled:  b.Spec.Enabled,
		},
	}
}
