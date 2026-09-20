package inference

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/adapters"
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
// list-models document a GET /{profile}/v1/models, on the same auth chain
// as the shape's inference routes. No adapter filter: relay translates
// cross-shape, so every model the key can see is reachable from the
// profile's inbound shape — the /openai/v1/models filter predates that and
// stays as it is.
func registerProfileModels(api huma.API, d Deps, mw huma.Middlewares) {
	for _, p := range d.Profiles.Profiles() {
		lister, ok := p.(clientprofile.ModelLister)
		if !ok {
			continue
		}
		name := p.Name()
		huma.Register(api, huma.Operation{
			OperationID: "list_models_" + name,
			Method:      http.MethodGet,
			Path:        "/" + name + "/v1/models",
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
			body, contentType, err := lister.Models(modelEntries(snap, pol, models))
			if err != nil {
				return nil, huma.Error500InternalServerError("render model list", err)
			}
			return &huma.StreamResponse{Body: func(hctx huma.Context) {
				hctx.SetHeader("Content-Type", contentType)
				_, _ = hctx.BodyWriter().Write(body)
			}}, nil
		})
	}
}

// modelEntries projects visible models into the neutral view a profile
// renders. One entry per addressable snapshot name; a model's aliases ride
// its pointer snapshot, which is what an alias resolves to. pol is nil on
// the policy-less path.
func modelEntries(snap *catalog.Snapshot, pol *policy.Policy, models []*model.Model) []clientprofile.ModelEntry {
	entries := make([]clientprofile.ModelEntry, 0, len(models))
	seen := map[string]struct{}{}
	for _, m := range models {
		hosts := modelHosts(snap, pol, m)
		for i := range m.Spec.Snapshots {
			s := &m.Spec.Snapshots[i]
			if _, dup := seen[s.Name]; dup {
				continue
			}
			seen[s.Name] = struct{}{}
			e := clientprofile.ModelEntry{ID: s.Name, DisplayName: m.Meta.DisplayName, Hosts: hosts}
			if e.DisplayName == "" {
				e.DisplayName = s.Name
			}
			if strings.EqualFold(s.Name, m.Spec.Pointer) {
				e.Aliases = exactAliases(m.Spec.Aliases)
			}
			entries = append(entries, e)
		}
	}
	return entries
}

// modelHosts lists the hosts the caller can actually route m to, carrying
// each binding's base-tier input/output rates when a pricing row resolves.
// A host outside the caller's grant must never reach the listing — the
// description would advertise a route the key cannot take.
func modelHosts(snap *catalog.Snapshot, pol *policy.Policy, m *model.Model) []clientprofile.ModelHost {
	var hosts []clientprofile.ModelHost
	for _, b := range snap.BindingsForModel(m.Meta.ID) {
		if !b.IsEnabled() {
			continue
		}
		switch {
		case pol != nil:
			if !routing.PolicyAllowsBinding(snap, pol, m, b) {
				continue
			}
		// Policy-less keys have no grant to consult, so the listing keeps
		// its own reachability rule: a host the relay holds credentials for.
		case len(snap.HostKeysForHost(b.Spec.HostID)) == 0:
			continue
		}
		name, ok := snap.HostSlug(b.Spec.HostID)
		if !ok {
			continue
		}
		h := clientprofile.ModelHost{Name: name}
		if p, ok := snap.PricingForBinding(b); ok {
			in, okIn := p.RateFor(pricing.MeterTokensInput, 0)
			out, okOut := p.RateFor(pricing.MeterTokensOutput, 0)
			if okIn && okOut {
				h.Priced = true
				h.InputUSDPerMtok = perMtok(in)
				h.OutputUSDPerMtok = perMtok(out)
			}
		}
		hosts = append(hosts, h)
	}
	return hosts
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
	snap, _, models, err := visibleModels(ctx, d, adapterFilter)
	if err != nil {
		return nil, err
	}
	out := &modelsOutput{}
	out.Body.Object = "list"
	seen := map[string]struct{}{}
	for _, m := range models {
		appendModelRows(&out.Body.Data, snap, m, seen)
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

// appendModelRows emits one row per Snapshot, deduplicating on id.
// Customer-facing addressability is purely via Snapshot.Name — the
// Model.Meta.Name slug is admin-only and never exposed here.
func appendModelRows(out *[]modelObject, snap *catalog.Snapshot, m *model.Model, seen map[string]struct{}) {
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

// modelHasReachableBinding returns true iff the model has at least one
// enabled host binding to a host with credentials, optionally restricted
// to a specific adapter kind.
func modelHasReachableBinding(snap *catalog.Snapshot, m *model.Model, adapterFilter adapters.Name) bool {
	for _, hb := range snap.BindingsForModel(m.Meta.ID) {
		if !hb.IsEnabled() {
			continue
		}
		if adapterFilter != "" && hb.Spec.Adapter != adapterFilter {
			continue
		}
		if len(snap.HostKeysForHost(hb.Spec.HostID)) == 0 {
			continue
		}
		return true
	}
	return false
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
