package inference

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/settings"
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
	rk := RelayKeyFromContext(ctx)
	if rk == nil {
		return nil, huma.Error401Unauthorized("missing relay key")
	}
	snap := d.Catalog.Current()
	out := &modelsOutput{}
	out.Body.Object = "list"

	if rk.Spec.PolicyID == "" {
		v, _ := d.Catalog.Setting(settings.SectionInference)
		cfg, _ := v.(*settings.Inference)
		if cfg == nil || !cfg.AllowMissingPolicy {
			return nil, huma.Error403Forbidden("policy-less traffic is disabled on this relay")
		}
		seen := map[string]struct{}{}
		for _, m := range snap.AllModels() {
			if !modelHasReachableBinding(snap, m, adapterFilter) {
				continue
			}
			appendModelRows(&out.Body.Data, snap, m, seen)
		}
		return out, nil
	}

	pol, ok := snap.Policy(rk.Spec.PolicyID)
	if !ok {
		return nil, huma.Error500InternalServerError("policy not found for relay key")
	}
	seen := map[string]struct{}{}
	for _, m := range snap.AllModels() {
		if !routing.PolicyAllows(snap, pol, m) {
			continue
		}
		if adapterFilter != "" && !modelHasAdapter(m, adapterFilter) {
			continue
		}
		appendModelRows(&out.Body.Data, snap, m, seen)
	}
	return out, nil
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
	for i := range m.Spec.Hosts {
		hb := &m.Spec.Hosts[i]
		if !hb.IsEnabled() {
			continue
		}
		if adapterFilter != "" && hb.Adapter != adapterFilter {
			continue
		}
		if len(snap.HostKeysForHost(hb.HostID)) == 0 {
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
func modelHasAdapter(m *model.Model, name adapters.Name) bool {
	for i := range m.Spec.Hosts {
		hb := &m.Spec.Hosts[i]
		if hb.IsEnabled() && hb.Adapter == name {
			return true
		}
	}
	return false
}

