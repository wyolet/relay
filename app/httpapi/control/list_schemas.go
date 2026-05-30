// list_schemas.go declares the per-resource filter allowlists consumed by
// the generic list handler (registerKind). Each schema is the single source
// of truth for that resource's filterable/sortable params: the accessors
// are typed closures, so renaming an underlying spec field is a compile
// error here. The query-param name, the allowlist entry, and the match
// logic all derive from one Field literal.
//
// Baseline coverage (this PR): the dimensions available on today's specs.
// Created/updated time filters depend on Metadata timestamps (separate PR);
// richer model capability filters and the host-key breaker-state filter
// land with the per-resource allowlist expansion.
package control

import (
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/pkg/filter"
)

// enabledTrue resolves the nil-means-enabled convention shared by every
// catalog spec's *bool Enabled field.
func enabledTrue(e *bool) bool { return e == nil || *e }

var policyFilter = filter.Schema[policy.Policy]{
	Fields: []filter.Field[policy.Policy]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(p *policy.Policy) string { return p.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(p *policy.Policy) bool { return enabledTrue(p.Spec.Enabled) }},
		{Name: "payload_logging", Kind: filter.Bool, GetBool: func(p *policy.Policy) bool { return p.Spec.PayloadLoggingEnabled }},
		{Name: "include_deprecated", Kind: filter.Bool, GetBool: func(p *policy.Policy) bool { return p.Spec.IncludeDeprecated }},
		{Name: "rate_limit_id", Kind: filter.String, Get: func(p *policy.Policy) string { return p.Spec.RateLimitID }},
		{Name: "key_selection", Kind: filter.String, Get: func(p *policy.Policy) string { return string(p.Spec.KeySelection) }},
		{Name: "model_id", Kind: filter.String, Repeat: true, GetMulti: func(p *policy.Policy) []string { return p.Spec.ModelIDs }},
		{Name: "host_key_id", Kind: filter.String, Repeat: true, GetMulti: func(p *policy.Policy) []string { return p.Spec.HostKeyIDs }},
	},
	Q:           func(p *policy.Policy) []string { return []string{p.Meta.Name, p.Meta.DisplayName, p.Meta.Description} },
	DefaultSort: "name",
}

var modelFilter = filter.Schema[model.Model]{
	Fields: []filter.Field[model.Model]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(m *model.Model) string { return m.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(m *model.Model) bool { return enabledTrue(m.Spec.Enabled) }},
		{Name: "family", Kind: filter.String, Repeat: true, Sortable: true, Get: func(m *model.Model) string { return m.Spec.Family }},
		{Name: "license", Kind: filter.String, Get: func(m *model.Model) string { return m.Spec.License }},
		{Name: "tag", Kind: filter.String, Repeat: true, GetMulti: func(m *model.Model) []string { return m.Spec.Tags }},
		{Name: "context_window", Kind: filter.Int, Sortable: true, GetInt: func(m *model.Model) int64 { return int64(m.Spec.ContextWindowTotal) }},
		{Name: "max_output_tokens", Kind: filter.Int, GetInt: func(m *model.Model) int64 { return int64(m.Spec.MaxOutputTokens) }},
	},
	Q: func(m *model.Model) []string {
		return append([]string{m.Meta.Name, m.Meta.DisplayName, m.Spec.Family}, m.Spec.Tags...)
	},
	DefaultSort: "name",
}

var hostFilter = filter.Schema[host.Host]{
	Fields: []filter.Field[host.Host]{
		{Name: "name", Kind: filter.String, Sortable: true, Get: func(h *host.Host) string { return h.Meta.Name }},
		{Name: "enabled", Kind: filter.Bool, GetBool: func(h *host.Host) bool { return enabledTrue(h.Spec.Enabled) }},
		{Name: "default_policy", Kind: filter.String, Get: func(h *host.Host) string { return h.Spec.DefaultPolicy }},
		{Name: "policy_id", Kind: filter.String, Repeat: true, GetMulti: func(h *host.Host) []string { return h.Spec.Policies }},
	},
	Q:           func(h *host.Host) []string { return []string{h.Meta.Name, h.Meta.DisplayName, h.Spec.BaseURL} },
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
	},
	Q:           func(k *relaykey.RelayKey) []string { return []string{k.Meta.Name, k.Meta.DisplayName, k.Spec.Prefix} },
	DefaultSort: "name",
}
