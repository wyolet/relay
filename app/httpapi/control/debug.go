// GET /debug/snapshot — render the current data-plane snapshot for
// operator inspection. The snapshot is what the hot path actually sees;
// diffing it against PG (via the regular admin list endpoints) is the
// fastest way to spot what sanitize dropped.
//
// detail=counts (default): per-kind row counts + per-policy reverse-join
//
//	sizes. Tiny payload, scans nothing.
//
// detail=full:             every entity in stable slug order, sanitized
//
//	spec included. Bigger; admin-only, no SLO.
package control

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
)

type debugSnapshotInput struct {
	Detail string `query:"detail" enum:"counts,full" default:"counts" doc:"counts = per-kind sizes only; full = every entity."`
}

type debugSnapshotCounts struct {
	Providers  int `json:"providers"`
	Hosts      int `json:"hosts"`
	Models     int `json:"models"`
	Policies   int `json:"policies"`
	HostKeys   int `json:"hostKeys"`
	RelayKeys  int `json:"relayKeys"`
	RateLimits int `json:"rateLimits"`
	Pricings   int `json:"pricings"`
}

type debugPolicyJoin struct {
	PolicyID    string `json:"policyId"`
	PolicyName  string `json:"policyName"`
	Models      int    `json:"models"`
	HostKeys    int    `json:"hostKeys"`
	RateLimited bool   `json:"rateLimited"`
}

type debugSnapshotBody struct {
	Counts      debugSnapshotCounts    `json:"counts"`
	PolicyJoins []debugPolicyJoin      `json:"policyJoins,omitempty"`
	Providers   []*provider.Provider   `json:"providers,omitempty"`
	Hosts       []*host.Host           `json:"hosts,omitempty"`
	Models      []*model.Model         `json:"models,omitempty"`
	Policies    []*policy.Policy       `json:"policies,omitempty"`
	HostKeys    []*hostkey.HostKey     `json:"hostKeys,omitempty"`
	RelayKeys   []*relaykey.RelayKey   `json:"relayKeys,omitempty"`
	RateLimits  []*ratelimit.RateLimit `json:"rateLimits,omitempty"`
	Pricings    []*pricing.Pricing     `json:"pricings,omitempty"`
}

type debugSnapshotOutput struct {
	Body debugSnapshotBody
}

func registerDebug(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "debug_snapshot",
		Method:      http.MethodGet,
		Path:        "/debug/snapshot",
		Summary:     "Dump the current data-plane snapshot",
		Description: "Returns the in-memory catalog snapshot the hot path " +
			"reads. Use to compare against PG (admin list endpoints) when " +
			"investigating why a row isn't reaching the data plane — the " +
			"sanitizer may have dropped it.",
		Tags:        []string{"debug"},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(ctx context.Context, in *debugSnapshotInput) (*debugSnapshotOutput, error) {
		snap := d.Catalog.Current()
		out := &debugSnapshotOutput{}

		policies := snap.AllPolicies()
		out.Body.Counts = debugSnapshotCounts{
			Providers:  len(snap.AllProviders()),
			Hosts:      len(snap.Hosts()),
			Models:     len(snap.AllModels()),
			Policies:   len(policies),
			HostKeys:   len(snap.AllHostKeys()),
			RelayKeys:  len(snap.AllRelayKeys()),
			RateLimits: len(snap.AllRateLimits()),
			Pricings:   len(snap.AllPricings()),
		}
		out.Body.PolicyJoins = make([]debugPolicyJoin, 0, len(policies))
		for _, p := range policies {
			out.Body.PolicyJoins = append(out.Body.PolicyJoins, debugPolicyJoin{
				PolicyID:    p.Meta.ID,
				PolicyName:  p.Meta.Name,
				Models:      len(snap.ModelsInPolicy(p.Meta.ID)),
				HostKeys:    len(snap.HostKeysInPolicy(p.Meta.ID)),
				RateLimited: snap.RateLimitOfPolicy(p.Meta.ID) != nil,
			})
		}

		if in.Detail == "full" {
			out.Body.Providers = snap.AllProviders()
			out.Body.Hosts = snap.Hosts()
			out.Body.Models = snap.AllModels()
			out.Body.Policies = policies
			out.Body.HostKeys = snap.AllHostKeys()
			out.Body.RelayKeys = snap.AllRelayKeys()
			out.Body.RateLimits = snap.AllRateLimits()
			out.Body.Pricings = snap.AllPricings()
		}
		return out, nil
	})
}
