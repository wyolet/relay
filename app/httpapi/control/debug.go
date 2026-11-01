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

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
)

type debugSnapshotInput struct {
	Detail string `query:"detail" enum:"counts,full" default:"counts" doc:"counts = per-kind sizes only; full = every entity."`
}

type debugSnapshotCounts struct {
	Providers  int `json:"providers"  doc:"Providers in the snapshot."`
	Hosts      int `json:"hosts"      doc:"Hosts in the snapshot."`
	Models     int `json:"models"     doc:"Models in the snapshot."`
	Policies   int `json:"policies"   doc:"Policies in the snapshot."`
	HostKeys   int `json:"hostKeys"   doc:"Host keys in the snapshot."`
	Keys       int `json:"keys"       doc:"Relay keys in the snapshot."`
	RateLimits int `json:"rateLimits" doc:"Rate limits in the snapshot."`
	Pricings   int `json:"pricings"   doc:"Pricing sheets in the snapshot."`
}

// debugPolicyJoin counts a policy's model refs and links, as stored.
type debugPolicyJoin struct {
	PolicyID    string `json:"policyId"    doc:"Policy id."`
	PolicyName  string `json:"policyName"  doc:"Policy slug."`
	Models      int    `json:"models"      doc:"Model references the policy lists."`
	Pools       int    `json:"pools"       doc:"Pools it links."`
	Credentials int    `json:"credentials" doc:"Host keys it links directly."`
	RateLimits  int    `json:"rateLimits"  doc:"Rate-limit links."`
	Budgets     int    `json:"budgets"     doc:"Budgets it links."`
}

type debugSnapshotBody struct {
	Counts      debugSnapshotCounts    `json:"counts"                doc:"Row counts per kind."`
	PolicyJoins []debugPolicyJoin      `json:"policyJoins,omitempty" doc:"Per-policy link counts."`
	Providers   []*provider.Provider   `json:"providers,omitempty"   doc:"Providers. Full dump only."`
	Hosts       []*host.Host           `json:"hosts,omitempty"       doc:"Hosts. Full dump only."`
	Models      []*model.Model         `json:"models,omitempty"      doc:"Models. Full dump only."`
	Policies    []*policy.Policy       `json:"policies,omitempty"    doc:"Policies. Full dump only."`
	HostKeys    []*hostkey.HostKey     `json:"hostKeys,omitempty"    doc:"Host keys, secret-free. Full dump only."`
	Keys        []*key.Key             `json:"keys,omitempty"        doc:"Relay keys. Full dump only."`
	RateLimits  []*ratelimit.RateLimit `json:"rateLimits,omitempty"  doc:"Rate limits. Full dump only."`
	Pricings    []*pricing.Pricing     `json:"pricings,omitempty"    doc:"Pricing sheets. Full dump only."`
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
		Errors:      []int{401, 403, 500},
	}, func(ctx context.Context, in *debugSnapshotInput) (*debugSnapshotOutput, error) {
		if err := d.Authz.Authorize(ctx, "debug.snapshot", authz.Resource{Kind: "debug"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		snap := d.Catalog.Current()
		out := &debugSnapshotOutput{}

		policies := snap.AllPolicies()
		out.Body.Counts = debugSnapshotCounts{
			Providers:  len(snap.AllProviders()),
			Hosts:      len(snap.Hosts()),
			Models:     len(snap.AllModels()),
			Policies:   len(policies),
			HostKeys:   len(snap.AllHostKeys()),
			Keys:       len(snap.AllKeys()),
			RateLimits: len(snap.AllRateLimits()),
			Pricings:   len(snap.AllPricings()),
		}
		out.Body.PolicyJoins = make([]debugPolicyJoin, 0, len(policies))
		for _, p := range policies {
			out.Body.PolicyJoins = append(out.Body.PolicyJoins, debugPolicyJoin{
				PolicyID:    p.Meta.ID,
				PolicyName:  p.Meta.Name,
				Models:      len(p.Spec.Models),
				Pools:       len(p.Spec.PoolIDs),
				Credentials: len(p.Spec.CredentialIDs),
				RateLimits:  len(p.Spec.RateLimits),
				Budgets:     len(p.Spec.BudgetIDs),
			})
		}

		if in.Detail == "full" {
			// Every row the data plane holds, keys and host keys included.
			audit.Record(ctx, "debug.snapshot", audit.Resource{Kind: "debug", Name: "full"}, audit.StatusAllowed)
			out.Body.Providers = snap.AllProviders()
			out.Body.Hosts = snap.Hosts()
			out.Body.Models = snap.AllModels()
			out.Body.Policies = policies
			out.Body.HostKeys = snap.AllHostKeys()
			out.Body.Keys = snap.AllKeys()
			out.Body.RateLimits = snap.AllRateLimits()
			out.Body.Pricings = snap.AllPricings()
		}
		return out, nil
	})
}
