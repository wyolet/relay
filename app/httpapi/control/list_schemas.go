// list_schemas.go declares the per-resource filter allowlists consumed by
// the generic list handler (registerKind). Each schema is the single source
// of truth for that resource's filterable/sortable params: the accessors
// are typed closures, so renaming an underlying spec field is a compile
// error here. The query-param name, the allowlist entry, and the match
// logic all derive from one Field literal.
//
// Covers all eight catalog kinds. Time filters (created/updated, release/
// deprecation dates) read Metadata timestamps + spec date strings; ?label=
// k=v works on every kind via the Labels hook. The host-key circuit-breaker
// state filter (?health=) is intentionally absent here — it needs a
// snapshot+kv join the pure store-slice engine can't express; see
// docs/filtering.md F3.
package control

import (
	"time"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/pkg/filter"
)

// enabledTrue resolves the nil-means-enabled convention shared by every
// catalog spec's *bool Enabled field.
func enabledTrue(e *bool) bool { return e == nil || *e }

func labelsOf(m meta.Metadata) map[string]string { return m.Labels }

// catalogDate parses a catalog date string (YYYY-MM-DD or RFC3339) to a
// time for range filtering. Unparseable/empty → zero, which never satisfies
// a _from/_to bound.
func catalogDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

// capabilityNames returns the names of the model's enabled capabilities.
// capabilityEnum is the closed set used to 400 on a typo'd ?capability=.
var capabilityEnum = []string{
	"chat", "embeddings", "streaming", "tools", "parallelTools", "vision",
	"audio", "promptCache", "reasoning", "jsonMode", "structuredOutputs", "batch",
}

func capabilityNames(c model.Capabilities) []string {
	var out []string
	for _, p := range []struct {
		on   bool
		name string
	}{
		{c.Chat, "chat"}, {c.Embeddings, "embeddings"}, {c.Streaming, "streaming"},
		{c.Tools, "tools"}, {c.ParallelTools, "parallelTools"}, {c.Vision, "vision"},
		{c.Audio, "audio"}, {c.PromptCache, "promptCache"}, {c.Reasoning, "reasoning"},
		{c.JSONMode, "jsonMode"}, {c.StructuredOutputs, "structuredOutputs"}, {c.Batch, "batch"},
	} {
		if p.on {
			out = append(out, p.name)
		}
	}
	return out
}

var policyFilter = filter.Schema[policy.Policy]{
	Fields: []filter.Field[policy.Policy]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(p *policy.Policy) string { return p.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(p *policy.Policy) bool { return enabledTrue(p.Spec.Enabled) }},
		{Name: "payload_logging", Kind: filter.Bool, GetBool: func(p *policy.Policy) bool { return p.Spec.PayloadLoggingEnabled }},
		{Name: "include_deprecated", Kind: filter.Bool, GetBool: func(p *policy.Policy) bool { return p.Spec.IncludeDeprecated }},
		{Name: "rate_limit_id", Kind: filter.String, Get: func(p *policy.Policy) string { return p.Spec.RateLimitID }},
		{Name: "key_selection", Kind: filter.String, Get: func(p *policy.Policy) string { return string(p.Spec.KeySelection) }},
		{Name: "owner", Kind: filter.String, Get: func(p *policy.Policy) string { return string(p.Meta.Owner.Kind) }},
		{Name: "model_id", Kind: filter.String, Repeat: true, GetMulti: func(p *policy.Policy) []string { return p.Spec.ModelIDs }},
		{Name: "host_key_id", Kind: filter.String, Repeat: true, GetMulti: func(p *policy.Policy) []string { return p.Spec.HostKeyIDs }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(p *policy.Policy) time.Time { return p.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(p *policy.Policy) time.Time { return p.Meta.UpdatedAt }},
	},
	Q:           func(p *policy.Policy) []string { return []string{p.Meta.Name, p.Meta.DisplayName, p.Meta.Description} },
	Labels:      func(p *policy.Policy) map[string]string { return labelsOf(p.Meta) },
	DefaultSort: "name",
}

var modelFilter = filter.Schema[model.Model]{
	Fields: []filter.Field[model.Model]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(m *model.Model) string { return m.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(m *model.Model) bool { return enabledTrue(m.Spec.Enabled) }},
		{Name: "deprecated", Kind: filter.Bool, GetBool: func(m *model.Model) bool {
			return m.Spec.Deprecation != nil || m.Spec.DeprecationDate != ""
		}},
		{Name: "family", Kind: filter.String, Repeat: true, Sortable: true, Get: func(m *model.Model) string { return m.Spec.Family }},
		{Name: "license", Kind: filter.String, Get: func(m *model.Model) string { return m.Spec.License }},
		{Name: "provider_id", Kind: filter.String, Repeat: true, Get: func(m *model.Model) string { return m.Meta.Owner.ID }},
		{Name: "tag", Kind: filter.String, Repeat: true, GetMulti: func(m *model.Model) []string { return m.Spec.Tags }},
		{Name: "capability", Kind: filter.String, Repeat: true, MatchAll: true, Enum: capabilityEnum,
			GetMulti: func(m *model.Model) []string { return capabilityNames(m.Spec.Capabilities) }},
		{Name: "modality", Kind: filter.String, Repeat: true, GetMulti: func(m *model.Model) []string {
			return append(append([]string{}, m.Spec.Modalities.Input...), m.Spec.Modalities.Output...)
		}},
		{Name: "context_window", Kind: filter.Int, Sortable: true, GetInt: func(m *model.Model) int64 { return int64(m.Spec.ContextWindowTotal) }},
		{Name: "max_output_tokens", Kind: filter.Int, GetInt: func(m *model.Model) int64 { return int64(m.Spec.MaxOutputTokens) }},
		{Name: "released", Kind: filter.Time, Sortable: true, GetTime: func(m *model.Model) time.Time { return catalogDate(m.Spec.ReleaseDate) }},
		{Name: "deprecated_date", Kind: filter.Time, GetTime: func(m *model.Model) time.Time { return catalogDate(m.Spec.DeprecationDate) }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(m *model.Model) time.Time { return m.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(m *model.Model) time.Time { return m.Meta.UpdatedAt }},
	},
	Q: func(m *model.Model) []string {
		return append([]string{m.Meta.Name, m.Meta.DisplayName, m.Spec.Family}, m.Spec.Tags...)
	},
	Labels:      func(m *model.Model) map[string]string { return labelsOf(m.Meta) },
	DefaultSort: "name",
}

var hostFilter = filter.Schema[host.Host]{
	Fields: []filter.Field[host.Host]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(h *host.Host) string { return h.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(h *host.Host) bool { return enabledTrue(h.Spec.Enabled) }},
		{Name: "has_default_policy", Kind: filter.Bool, GetBool: func(h *host.Host) bool { return h.Spec.DefaultPolicy != "" }},
		{Name: "default_policy", Kind: filter.String, Get: func(h *host.Host) string { return h.Spec.DefaultPolicy }},
		{Name: "policy_id", Kind: filter.String, Repeat: true, GetMulti: func(h *host.Host) []string { return h.Spec.Policies }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(h *host.Host) time.Time { return h.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(h *host.Host) time.Time { return h.Meta.UpdatedAt }},
	},
	Q:           func(h *host.Host) []string { return []string{h.Meta.Name, h.Meta.DisplayName, h.Spec.BaseURL} },
	Labels:      func(h *host.Host) map[string]string { return labelsOf(h.Meta) },
	DefaultSort: "name",
}

var hostKeyFilter = filter.Schema[hostkey.HostKey]{
	Fields: []filter.Field[hostkey.HostKey]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(k *hostkey.HostKey) string { return k.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(k *hostkey.HostKey) bool { return enabledTrue(k.Spec.Enabled) }},
		{Name: "host_id", Kind: filter.String, Repeat: true, Get: func(k *hostkey.HostKey) string { return k.Spec.HostID }},
		{Name: "policy_id", Kind: filter.String, Repeat: true, Get: func(k *hostkey.HostKey) string { return k.Spec.PolicyID }},
		{Name: "default_tier", Kind: filter.String, Get: func(k *hostkey.HostKey) string { return k.Spec.DefaultTier }},
		{Name: "value_kind", Kind: filter.String, Enum: []string{"env", "stored"}, Get: func(k *hostkey.HostKey) string { return string(k.Spec.ValueFrom.Kind) }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(k *hostkey.HostKey) time.Time { return k.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(k *hostkey.HostKey) time.Time { return k.Meta.UpdatedAt }},
	},
	Q:           func(k *hostkey.HostKey) []string { return []string{k.Meta.Name, k.Meta.DisplayName} },
	Labels:      func(k *hostkey.HostKey) map[string]string { return labelsOf(k.Meta) },
	DefaultSort: "name",
}

var providerFilter = filter.Schema[provider.Provider]{
	Fields: []filter.Field[provider.Provider]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(p *provider.Provider) string { return p.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(p *provider.Provider) bool { return enabledTrue(p.Spec.Enabled) }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(p *provider.Provider) time.Time { return p.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(p *provider.Provider) time.Time { return p.Meta.UpdatedAt }},
	},
	Q: func(p *provider.Provider) []string {
		return []string{p.Meta.Name, p.Meta.DisplayName, p.Meta.Description}
	},
	Labels:      func(p *provider.Provider) map[string]string { return labelsOf(p.Meta) },
	DefaultSort: "name",
}

var pricingFilter = filter.Schema[pricing.Pricing]{
	Fields: []filter.Field[pricing.Pricing]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(p *pricing.Pricing) string { return p.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(p *pricing.Pricing) bool { return enabledTrue(p.Spec.Enabled) }},
		{Name: "currency", Kind: filter.String, Get: func(p *pricing.Pricing) string { return p.Spec.Currency }},
		{Name: "target_model_id", Kind: filter.String, Repeat: true, GetMulti: func(p *pricing.Pricing) []string { return p.Spec.TargetModelIDs }},
		{Name: "meter", Kind: filter.String, Repeat: true, GetMulti: func(p *pricing.Pricing) []string {
			out := make([]string, 0, len(p.Spec.Rates))
			for _, r := range p.Spec.Rates {
				out = append(out, string(r.Meter))
			}
			return out
		}},
		{Name: "unit", Kind: filter.String, Repeat: true, GetMulti: func(p *pricing.Pricing) []string {
			out := make([]string, 0, len(p.Spec.Rates))
			for _, r := range p.Spec.Rates {
				out = append(out, string(r.Unit))
			}
			return out
		}},
		{Name: "has_tiers", Kind: filter.Bool, GetBool: func(p *pricing.Pricing) bool {
			for _, r := range p.Spec.Rates {
				if r.AboveTokens > 0 {
					return true
				}
			}
			return false
		}},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(p *pricing.Pricing) time.Time { return p.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(p *pricing.Pricing) time.Time { return p.Meta.UpdatedAt }},
	},
	Q:           func(p *pricing.Pricing) []string { return []string{p.Meta.Name, p.Meta.DisplayName} },
	Labels:      func(p *pricing.Pricing) map[string]string { return labelsOf(p.Meta) },
	DefaultSort: "name",
}

var bindingFilter = filter.Schema[binding.Binding]{
	Fields: []filter.Field[binding.Binding]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(b *binding.Binding) string { return b.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(b *binding.Binding) bool { return enabledTrue(b.Spec.Enabled) }},
		{Name: "model_id", Kind: filter.String, Get: func(b *binding.Binding) string { return b.Spec.ModelID }},
		{Name: "host_id", Kind: filter.String, Get: func(b *binding.Binding) string { return b.Spec.HostID }},
		{Name: "pricing_id", Kind: filter.String, Get: func(b *binding.Binding) string { return b.Spec.PricingID }},
		{Name: "adapter", Kind: filter.String, Get: func(b *binding.Binding) string { return string(b.Spec.Adapter) }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(b *binding.Binding) time.Time { return b.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(b *binding.Binding) time.Time { return b.Meta.UpdatedAt }},
	},
	Q:           func(b *binding.Binding) []string { return []string{b.Meta.Name, b.Meta.DisplayName} },
	Labels:      func(b *binding.Binding) map[string]string { return labelsOf(b.Meta) },
	DefaultSort: "name",
}

var rateLimitFilter = filter.Schema[ratelimit.RateLimit]{
	Fields: []filter.Field[ratelimit.RateLimit]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(r *ratelimit.RateLimit) string { return r.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(r *ratelimit.RateLimit) bool { return enabledTrue(r.Spec.Enabled) }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(r *ratelimit.RateLimit) time.Time { return r.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(r *ratelimit.RateLimit) time.Time { return r.Meta.UpdatedAt }},
	},
	Q: func(r *ratelimit.RateLimit) []string {
		return []string{r.Meta.Name, r.Meta.DisplayName, r.Meta.Description}
	},
	Labels:      func(r *ratelimit.RateLimit) map[string]string { return labelsOf(r.Meta) },
	DefaultSort: "name",
}

var relayKeyFilter = filter.Schema[relaykey.RelayKey]{
	Fields: []filter.Field[relaykey.RelayKey]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(k *relaykey.RelayKey) string { return k.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(k *relaykey.RelayKey) bool { return enabledTrue(k.Spec.Enabled) }},
		{Name: "revoked", Kind: filter.Bool, GetBool: func(k *relaykey.RelayKey) bool { return k.Spec.RevokedAt != nil }},
		{Name: "passthrough", Kind: filter.Bool, GetBool: func(k *relaykey.RelayKey) bool { return k.Spec.PassthroughAllowed }},
		{Name: "payload_logging", Kind: filter.Bool, GetBool: func(k *relaykey.RelayKey) bool { return k.Spec.PayloadLoggingEnabled }},
		{Name: "policy_id", Kind: filter.String, Repeat: true, Get: func(k *relaykey.RelayKey) string { return k.Spec.PolicyID }},
		{Name: "prefix", Kind: filter.String, Get: func(k *relaykey.RelayKey) string { return k.Spec.Prefix }},
		{Name: "created", Kind: filter.Time, Sortable: true, GetTime: func(k *relaykey.RelayKey) time.Time { return k.Meta.CreatedAt }},
		{Name: "updated", Kind: filter.Time, Sortable: true, GetTime: func(k *relaykey.RelayKey) time.Time { return k.Meta.UpdatedAt }},
	},
	Q:           func(k *relaykey.RelayKey) []string { return []string{k.Meta.Name, k.Meta.DisplayName, k.Spec.Prefix} },
	Labels:      func(k *relaykey.RelayKey) map[string]string { return labelsOf(k.Meta) },
	DefaultSort: "name",
}
