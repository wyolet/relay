package manifest

import (
	"fmt"
	"strings"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
)

// ---------------------------------------------------------------------------
// Policy
// ---------------------------------------------------------------------------

func ToPolicy(d PolicyDTO, idx Resolver) (*policy.Policy, error) {
	// Canonicalised exactly as the control API does, or apply reports an
	// update on every run for a grant the API wrote.
	models, err := policy.CanonicalizeModelRefs(d.Spec.Models)
	if err != nil {
		return nil, fmt.Errorf("policy %q: models: %w", d.Metadata.Name, err)
	}

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
		bModels, err := policy.CanonicalizeModelRefs(append([]string{}, b.Models...))
		if err != nil {
			return nil, fmt.Errorf("policy %q: rlBindings[%d].models: %w", d.Metadata.Name, i, err)
		}
		rlBindings = append(rlBindings, policy.RLBinding{
			Models:      bModels,
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
			IncludeDeprecated:     d.Spec.IncludeDeprecated,
			Enabled:               d.Spec.Enabled,
			PayloadLoggingEnabled: d.Spec.PayloadLoggingEnabled,
		},
	}, nil
}

func FromPolicy(p *policy.Policy, rev ReverseResolver) PolicyDTO {
	// Spec.Models is already in wire form (ref strings). Spec.ModelIDs is the
	// legacy literal-ID grant; emit those rows as model refs, not bare model
	// slugs, because a bare modelref token means "provider". Only when Models
	// is empty, though: the two coexist on a row, and rendering both would
	// make a re-apply of the export widen the grant.
	models := make([]string, 0, len(p.Spec.Models)+len(p.Spec.ModelIDs))
	models = append(models, p.Spec.Models...)
	if len(models) == 0 {
		for _, id := range p.Spec.ModelIDs {
			models = append(models, legacyModelRef(id, rev))
		}
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
