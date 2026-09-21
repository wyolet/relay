package inference

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/ids"
)

// modelObject is the OpenAI list-models entry shape.
type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelsOutput struct {
	Body struct {
		Object string        `json:"object"`
		Data   []modelObject `json:"data"`
	}
}

// registerModels serves GET /v1/models and GET /openai/v1/models — the
// list of models accessible to the authenticated relay key. The /openai
// namespace additionally filters out any model that has no enabled host
// binding declaring `adapter: openai`, since the OpenAI SDK can't reach
// those even if policy allows them.
//
// Policy-bound key: enumerates every enabled model and asks
// routing.PolicyAllows. Covers literal ModelIDs grants, modelref
// Spec.Models grants, and the implicit-wildcard case (both fields empty).
//
// Policy-less key (Spec.PolicyID empty + settings.Inference.
// AllowMissingPolicy on): returns every enabled model that has at least
// one enabled host binding to a host the relay has hostkeys for.
func registerModels(api huma.API, d Deps, mw huma.Middlewares) {
	registerModelsAt(api, d, mw, "/v1/models", "")
	registerModelsAt(api, d, mw, "/openai/v1/models", adapters.OpenAI)
	registerProfileModels(api, d, mw)
}

// registerProfileModels gives every profile that renders its own
// list-models document a GET under its /{profile} prefix, on the same auth
// chain as the shape's inference routes. No adapter filter: relay
// translates cross-shape, so every model the key can see is reachable from
// the profile's inbound shape — the /openai/v1/models filter predates that
// and stays as it is.
func registerProfileModels(api huma.API, d Deps, mw huma.Middlewares) {
	for _, p := range d.Profiles.Profiles() {
		render, ok := modelListRenderer(p)
		if !ok {
			continue
		}
		name := p.Name()
		huma.Register(api, huma.Operation{
			OperationID: "list_models_" + name,
			Method:      http.MethodGet,
			Path:        "/" + name + modelListPath(p),
			Summary:     "List models in the " + name + " client's own shape",
			Tags:        []string{"inference"},
			Middlewares: mw,
			Hidden:      true,
			Errors:      []int{401, 403, 500},
		}, func(ctx context.Context, _ *struct{}) (*huma.StreamResponse, error) {
			snap, pol, models, err := visibleModels(ctx, d, "")
			if err != nil {
				return nil, err
			}
			entries := modelEntries(snap, pol, models)
			return &huma.StreamResponse{Body: func(hctx huma.Context) {
				r, w := humachi.Unwrap(hctx)
				body, contentType, err := render(clientprofile.ListContext{PublicURL: publicInferenceURL(d, r)}, entries)
				if err != nil {
					WriteAPIError(w, http.StatusInternalServerError, "api_error", "render_models", err.Error())
					return
				}
				w.Header().Set("Content-Type", contentType)
				_, _ = w.Write(body)
			}}, nil
		})
	}
}

// modelListRenderer picks the profile's list projection, preferring the context-carrying form: a document that has to name the endpoint its client should call cannot be rendered from the entries alone.
func modelListRenderer(p clientprofile.Profile) (func(clientprofile.ListContext, []clientprofile.ModelEntry) ([]byte, string, error), bool) {
	if l, ok := p.(clientprofile.ListerWithContext); ok {
		return l.ModelsWithContext, true
	}
	if l, ok := p.(clientprofile.ModelLister); ok {
		return func(_ clientprofile.ListContext, entries []clientprofile.ModelEntry) ([]byte, string, error) {
			return l.Models(entries)
		}, true
	}
	return nil, false
}

// modelListPath is where the profile's client reads its model list, relative to the /{profile} prefix.
func modelListPath(p clientprofile.Profile) string {
	if r, ok := p.(clientprofile.ModelListRoute); ok {
		return r.ModelListPath()
	}
	return "/v1/models"
}

// publicInferenceURL is the base a client should send inference to: the deployment's declared public URL, or — when it declares none — the origin this very request arrived at, which is right for a direct caller and for any proxy that forwards the scheme.
func publicInferenceURL(d Deps, r *http.Request) string {
	if d.PublicURL != "" {
		return strings.TrimSuffix(d.PublicURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fwd := r.Header.Get("X-Forwarded-Proto"); fwd != "" {
		scheme = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	return scheme + "://" + r.Host
}

// modelEntries projects visible models into the neutral view a profile
// renders. One entry per addressable snapshot name; a model's aliases ride
// its pointer snapshot, which is what an alias resolves to. pol is nil on
// the policy-less path.
func modelEntries(snap *catalog.Snapshot, pol *policy.Policy, models []*model.Model) []clientprofile.ModelEntry {
	entries := make([]clientprofile.ModelEntry, 0, len(models))
	seen := map[string]struct{}{}
	for _, m := range models {
		granted := grantedBindings(snap, pol, m, "")
		for i := range m.Spec.Snapshots {
			s := &m.Spec.Snapshots[i]
			if _, dup := seen[s.Name]; dup {
				continue
			}
			hosts := snapshotHosts(snap, granted, s.Name)
			if len(hosts) == 0 {
				continue
			}
			seen[s.Name] = struct{}{}
			e := clientprofile.ModelEntry{
				ID:              s.Name,
				Model:           m.Meta.Name,
				DisplayName:     m.Meta.DisplayName,
				Hosts:           hosts,
				ContextWindow:   contextWindow(m),
				MaxOutputTokens: m.Spec.MaxOutputTokens,
				Reasoning:       m.Spec.Capabilities.Reasoning,
				ToolCall:        m.Spec.Capabilities.Tools,
				Vision:          m.Spec.Capabilities.Vision,
				Temperature:     temperatureSupported(m),
				Modalities: clientprofile.Modalities{
					Input:  m.Spec.Modalities.Input,
					Output: m.Spec.Modalities.Output,
				},
				ReleasedAt: s.ReleasedAt,
			}
			if e.DisplayName == "" {
				e.DisplayName = s.Name
			}
			if strings.EqualFold(s.Name, m.Spec.Pointer) {
				e.Pointer = true
				e.Aliases = exactAliases(m.Spec.Aliases)
			}
			entries = append(entries, e)
		}
	}
	return entries
}

// contextWindow is the window a client should size a session against: ContextWindowTotal is the canonical field, ContextWindowInput the fallback for models that publish only the split. 0 = the catalog declares none.
func contextWindow(m *model.Model) int {
	if m.Spec.ContextWindowTotal > 0 {
		return m.Spec.ContextWindowTotal
	}
	return m.Spec.ContextWindowInput
}

// temperatureSupported reports whether a caller may send a temperature. The catalog states the negative — UnsupportedParams lists what the upstream rejects for this model — and a picker needs the positive.
func temperatureSupported(m *model.Model) bool {
	for _, p := range m.Spec.Capabilities.UnsupportedParams {
		if p == "temperature" {
			return false
		}
	}
	return true
}

// grantedBindings keeps the model's enabled bindings the caller may actually route to. A binding outside the caller's grant must never reach a listing — the row would advertise a route the key cannot take. adapterFilter, when set, additionally keeps only bindings declaring it.
func grantedBindings(snap *catalog.Snapshot, pol *policy.Policy, m *model.Model, adapterFilter adapters.Name) []*binding.Binding {
	var out []*binding.Binding
	for _, b := range snap.BindingsForModel(m.Meta.ID) {
		if !b.IsEnabled() {
			continue
		}
		if adapterFilter != "" && b.Spec.Adapter != adapterFilter {
			continue
		}
		switch {
		case pol != nil:
			if !routing.PolicyAllowsBinding(snap, pol, m, b) {
				continue
			}
		// Policy-less keys have no grant to consult, so the listing keeps its own reachability rule: a host the relay holds credentials for, or one that needs none (routing injects the anonymous key there).
		case len(snap.HostKeysForHost(b.Spec.HostID)) == 0:
			if h, ok := snap.Host(b.Spec.HostID); !ok || !h.Spec.NoAuth {
				continue
			}
		}
		out = append(out, b)
	}
	return out
}

// servedBy reports whether any of the bindings serves the snapshot name — the same gate routing applies when it picks a binding, so a snapshot no binding serves is not addressable and must not be listed.
func servedBy(bindings []*binding.Binding, snapshotName string) bool {
	for _, b := range bindings {
		if b.Serves(snapshotName) {
			return true
		}
	}
	return false
}

// snapshotHosts lists the hosts serving one snapshot name, carrying each
// binding's base-tier input/output rates when a pricing row resolves.
func snapshotHosts(snap *catalog.Snapshot, bindings []*binding.Binding, snapshotName string) []clientprofile.ModelHost {
	var hosts []clientprofile.ModelHost
	for _, b := range bindings {
		if !b.Serves(snapshotName) {
			continue
		}
		name, ok := snap.HostSlug(b.Spec.HostID)
		if !ok {
			continue
		}
		h := clientprofile.ModelHost{Name: name}
		if p, ok := snap.PricingForBinding(b); ok {
			applyRates(&h, p)
		}
		hosts = append(hosts, h)
	}
	return hosts
}

// applyRates copies a binding's rate sheet onto the listing host: the base tier first — a host counts as priced only when both sides of a request have a rate — then the cache meters and the above-base tiers.
func applyRates(h *clientprofile.ModelHost, p *pricing.Pricing) {
	in, okIn := p.RateFor(pricing.MeterTokensInput, 0)
	out, okOut := p.RateFor(pricing.MeterTokensOutput, 0)
	if !okIn || !okOut {
		return
	}
	h.Priced = true
	h.InputUSDPerMtok = perMtok(in)
	h.OutputUSDPerMtok = perMtok(out)
	h.CacheReadUSDPerMtok = rateAt(p, pricing.MeterTokensCacheRead, 0)
	h.CacheWriteUSDPerMtok = rateAt(p, pricing.MeterTokensCacheCreation, 0)
	for _, above := range tierThresholds(p) {
		tier := clientprofile.PriceTier{
			AboveTokens:          above,
			CacheReadUSDPerMtok:  rateAt(p, pricing.MeterTokensCacheRead, above),
			CacheWriteUSDPerMtok: rateAt(p, pricing.MeterTokensCacheCreation, above),
		}
		if r, ok := p.RateFor(pricing.MeterTokensInput, above); ok {
			tier.InputUSDPerMtok = perMtok(r)
		}
		if r, ok := p.RateFor(pricing.MeterTokensOutput, above); ok {
			tier.OutputUSDPerMtok = perMtok(r)
		}
		h.Tiers = append(h.Tiers, tier)
	}
}

// tierThresholds lists the above-base thresholds the sheet declares, ascending and deduplicated across meters: one meter's tier row raises the price of the whole request past that point, so every meter is re-read there.
func tierThresholds(p *pricing.Pricing) []int {
	var out []int
	seen := map[int]struct{}{}
	for _, r := range p.Spec.Rates {
		if r.AboveTokens <= 0 {
			continue
		}
		if _, dup := seen[r.AboveTokens]; dup {
			continue
		}
		seen[r.AboveTokens] = struct{}{}
		out = append(out, r.AboveTokens)
	}
	sort.Ints(out)
	return out
}

// rateAt returns the meter's rate at a token count, nil when the sheet prices no such meter — nil is "unpriced", never "free".
func rateAt(p *pricing.Pricing, meter pricing.Meter, tokens int) *float64 {
	r, ok := p.RateFor(meter, tokens)
	if !ok {
		return nil
	}
	v := perMtok(r)
	return &v
}

// perMtok normalizes a rate to USD per million tokens.
func perMtok(r *pricing.Rate) float64 {
	if r.Unit == pricing.UnitPerUnit {
		return r.Amount * 1_000_000
	}
	return r.Amount
}

// exactAliases drops wildcard alias patterns: they match at resolution
// time but cannot be enumerated as list entries.
func exactAliases(aliases []string) []string {
	var out []string
	for _, a := range aliases {
		if _, _, isPattern := model.AliasPattern(a); isPattern {
			continue
		}
		out = append(out, a)
	}
	return out
}

// registerModelsAt registers a single list-models endpoint at path. If
// adapterFilter is non-empty, only models with at least one enabled host
// binding declaring that adapter are returned.
func registerModelsAt(api huma.API, d Deps, mw huma.Middlewares, path string, adapterFilter adapters.Name) {
	opID := "list_models"
	summary := "List models accessible to the caller (OpenAI-compatible)"
	if adapterFilter != "" {
		opID = "list_models_" + string(adapterFilter)
		summary = "List models reachable via the " + string(adapterFilter) + " wire shape"
	}
	huma.Register(api, huma.Operation{
		OperationID: opID,
		Method:      "GET",
		Path:        path,
		Summary:     summary,
		Tags:        []string{"inference"},
		Middlewares: mw,
		Errors:      []int{401, 403, 500},
	}, func(ctx context.Context, _ *struct{}) (*modelsOutput, error) {
		return listModels(ctx, d, adapterFilter)
	})
}

func listModels(ctx context.Context, d Deps, adapterFilter adapters.Name) (*modelsOutput, error) {
	snap, pol, models, err := visibleModels(ctx, d, adapterFilter)
	if err != nil {
		return nil, err
	}
	out := &modelsOutput{}
	out.Body.Object = "list"
	seen := map[string]struct{}{}
	for _, m := range models {
		appendModelRows(&out.Body.Data, snap, m, grantedBindings(snap, pol, m, adapterFilter), seen)
	}
	return out, nil
}

// visibleModels returns the models the authenticated relay key may list,
// along with the snapshot they were read from (so a caller projects off one
// consistent view) and the key's policy, nil for a policy-less key.
// adapterFilter, when set, keeps only models with an enabled binding
// declaring it.
func visibleModels(ctx context.Context, d Deps, adapterFilter adapters.Name) (*catalog.Snapshot, *policy.Policy, []*model.Model, error) {
	rk := RelayKeyFromContext(ctx)
	if rk == nil {
		return nil, nil, nil, huma.Error401Unauthorized("missing relay key")
	}
	snap := d.Catalog.Current()

	if rk.Spec.PolicyID == "" {
		v, _ := d.Catalog.Setting(settings.SectionInference)
		cfg, _ := v.(*settings.Inference)
		if cfg == nil || !cfg.AllowMissingPolicy {
			return nil, nil, nil, huma.Error403Forbidden("policy-less traffic is disabled on this relay")
		}
		var out []*model.Model
		for _, m := range snap.AllModels() {
			if modelHasReachableBinding(snap, m, adapterFilter) {
				out = append(out, m)
			}
		}
		return snap, nil, out, nil
	}

	pol, ok := snap.Policy(rk.Spec.PolicyID)
	if !ok {
		return nil, nil, nil, huma.Error500InternalServerError("policy not found for relay key")
	}
	var out []*model.Model
	for _, m := range snap.AllModels() {
		if !routing.PolicyAllows(snap, pol, m) {
			continue
		}
		if adapterFilter != "" && !modelHasAdapter(snap, m, adapterFilter) {
			continue
		}
		out = append(out, m)
	}
	return snap, pol, out, nil
}

// appendModelRows emits one row per Snapshot that at least one granted
// binding serves, deduplicating on id. Customer-facing addressability is
// purely via Snapshot.Name — the Model.Meta.Name slug is admin-only and
// never exposed here.
func appendModelRows(out *[]modelObject, snap *catalog.Snapshot, m *model.Model, granted []*binding.Binding, seen map[string]struct{}) {
	ownedBy := ""
	if pname, ok := snap.ProviderSlug(m.Meta.Owner.ID); ok {
		ownedBy = pname
	}
	modelCreated := ids.UnixSec(m.Meta.ID)

	for i := range m.Spec.Snapshots {
		s := &m.Spec.Snapshots[i]
		if _, dup := seen[s.Name]; dup {
			continue
		}
		if !servedBy(granted, s.Name) {
			continue
		}
		seen[s.Name] = struct{}{}
		*out = append(*out, modelObject{
			ID:      s.Name,
			Object:  "model",
			Created: snapshotCreated(s, modelCreated),
			OwnedBy: ownedBy,
		})
	}
}

// snapshotCreated returns ReleasedAt parsed as midnight UTC if available,
// else falls back to the owning Model's creation timestamp.
func snapshotCreated(s *model.Snapshot, fallback int64) int64 {
	if s.ReleasedAt == "" {
		return fallback
	}
	t, err := time.Parse("2006-01-02", s.ReleasedAt)
	if err != nil {
		return fallback
	}
	return t.UTC().Unix()
}

// modelHasReachableBinding reports whether a policy-less caller can route m at all: the same reachability rule grantedBindings applies with no policy, optionally restricted to one adapter kind.
func modelHasReachableBinding(snap *catalog.Snapshot, m *model.Model, adapterFilter adapters.Name) bool {
	return len(grantedBindings(snap, nil, m, adapterFilter)) > 0
}

// modelHasAdapter returns true iff the model has at least one enabled
// binding declaring kind, regardless of credentials. Used for policy-bound
// keys where the routing layer will surface a no-keys error rather than
// silently hiding the model.
func modelHasAdapter(snap *catalog.Snapshot, m *model.Model, name adapters.Name) bool {
	for _, hb := range snap.BindingsForModel(m.Meta.ID) {
		if hb.IsEnabled() && hb.Spec.Adapter == name {
			return true
		}
	}
	return false
}
