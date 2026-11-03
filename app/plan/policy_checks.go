// Policy checks report what a policy's own links leave unserved: linked pools that all serve nothing, a pool whose host serves none of its models, model entries no linked pool reaches or that name a disabled model, and rate limits scoped only to models it does not allow. They attach issues to the policy and build nothing, so no plan the request path reads depends on them.

package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/engine"
)

// policyCheck is the kind of the policy checks' output, which they never build: a check only attaches issues to the policy.
type policyCheck struct{}

// modelEntry is one parsed entry of a policy's model list and its index there.
type modelEntry struct {
	i   int
	ref modelref.Ref
}

// linkedPool is one existing pool a policy links, its index in poolIds and its plan (nil while it has none).
type linkedPool struct {
	i    int
	pool *pool.Pool
	plan *PoolPlan
}

func (c *controllers) policyCheck(tx *engine.Txn, id string) error {
	if p := engine.Get[policy.Policy](tx, id); p != nil {
		checkPolicy(tx, p)
	}
	engine.Drop(tx)
	return nil
}

func checkPolicy(tx *engine.Txn, p *policy.Policy) {
	src := engine.Ref{Kind: KindPolicy, ID: p.Meta.ID}
	entries := modelEntries(p.Spec.Models)
	var pools []linkedPool
	for i, pid := range p.Spec.PoolIDs {
		// A missing pool carries ref_missing on the policy already.
		if pl := engine.Get[pool.Pool](tx, pid); pl != nil {
			pools = append(pools, linkedPool{i: i, pool: pl, plan: engine.Get[PoolPlan](tx, pid)})
		}
	}
	checkPoolsServe(tx, src, pools)
	checkRateLimitScopes(tx, src, p, entries)
	if len(entries) == 0 {
		return
	}
	idx := engine.Get[CatalogIndex](tx, "")
	if idx == nil {
		return
	}
	checkModelEntries(tx, src, idx, entries, pools)
	checkPoolHosts(tx, src, idx, entries, pools)
}

// modelEntries parses a model list, leaving out the entries that cannot match (the expanded policy reports those).
func modelEntries(raw []string) []modelEntry {
	var out []modelEntry
	for i, s := range raw {
		if strings.HasPrefix(s, "group:") {
			continue
		}
		if r, err := modelref.Parse(s); err == nil {
			out = append(out, modelEntry{i: i, ref: r})
		}
	}
	return out
}

func checkPoolsServe(tx *engine.Txn, src engine.Ref, pools []linkedPool) {
	if len(pools) == 0 {
		return
	}
	names := make([]string, 0, len(pools))
	for _, lp := range pools {
		if lp.plan != nil && lp.plan.serves() {
			return
		}
		names = append(names, lp.pool.Meta.Name)
	}
	tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: IssuePoolsServeNothing, Source: src, Field: "spec.poolIds",
		Message: "none of its pools (" + strings.Join(names, ", ") + ") has a usable key at an enabled host; requests under this policy cannot be served"})
}

// checkRateLimitScopes files rate_limit_scope_outside_models on each scoped rate-limit link none of whose models the policy's model list allows: requests for them are refused before the limit counts.
func checkRateLimitScopes(tx *engine.Txn, src engine.Ref, p *policy.Policy, entries []modelEntry) {
	if len(entries) == 0 {
		return
	}
	for i, l := range p.Spec.RateLimits {
		scope := modelEntries(l.Models)
		if len(scope) == 0 || slices.ContainsFunc(scope, func(s modelEntry) bool {
			return slices.ContainsFunc(entries, func(e modelEntry) bool { return modelref.Overlap(s.ref, e.ref) })
		}) {
			continue
		}
		rl := engine.Get[ratelimit.RateLimit](tx, l.RateLimitID)
		if rl == nil {
			continue
		}
		tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueRateLimitScopeOutsideModels, Source: src, Field: fmt.Sprintf("spec.rateLimits[%d].models", i), Target: engine.Ref{Kind: KindRateLimit, ID: rl.Meta.ID},
			Message: "rate limit " + rl.Meta.Name + " applies to " + strings.Join(l.Models, ", ") + ", which this policy does not allow; it never counts"})
	}
}

// checkModelEntries files model_ref_disabled on each entry naming only a disabled model and, when the policy links pools, model_ref_unreachable on each entry served at none of their enabled hosts: an error when no entry is reachable.
func checkModelEntries(tx *engine.Txn, src engine.Ref, idx *CatalogIndex, entries []modelEntry, pools []linkedPool) {
	var hosts []*host.Host
	for _, lp := range pools {
		if h, ok := idx.Snap.Host(lp.pool.Spec.HostID); ok && servesRequests(h) && !slices.Contains(hosts, h) {
			hosts = append(hosts, h)
		}
	}
	var unreached []modelEntry
	reached := false
	for _, e := range entries {
		if m := disabledModel(idx, e.ref); m != nil {
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueModelRefDisabled, Source: src, Field: fmt.Sprintf("spec.models[%d]", e.i), Target: engine.Ref{Kind: KindModel, ID: m.Meta.ID},
				Message: "model " + e.ref.Raw + " is disabled; this entry serves nothing"})
			continue
		}
		if servedAtAny(idx, e.ref, hosts) {
			reached = true
		} else {
			unreached = append(unreached, e)
		}
	}
	// With no linked pool at an enabled host, pools_serve_nothing and host_disabled say why.
	if len(pools) == 0 || len(hosts) == 0 {
		return
	}
	sev := engine.SeverityWarning
	if !reached {
		sev = engine.SeverityError
	}
	names := make([]string, len(hosts))
	for i, h := range hosts {
		names[i] = h.Meta.Name
	}
	for _, e := range unreached {
		tx.Issue(engine.Issue{Severity: sev, Code: IssueModelRefUnreachable, Source: src, Field: fmt.Sprintf("spec.models[%d]", e.i),
			Message: e.ref.Raw + " is not served at any host the policy's pools reach (" + strings.Join(names, ", ") + ")"})
	}
}

// disabledModel returns the model ref names when every catalog model it names is disabled; nil for a wildcard entry, one naming an enabled model, or one naming none.
func disabledModel(idx *CatalogIndex, r modelref.Ref) *model.Model {
	if r.ModelWildcard {
		return nil
	}
	var off *model.Model
	for _, m := range idx.Snap.ModelsByName(r.Model) {
		if prov, _ := idx.Snap.ProviderSlug(m.Meta.Owner.ID); !r.ProviderWildcard && prov != r.Provider {
			continue
		}
		if m.IsEnabled() {
			return nil
		}
		off = m
	}
	return off
}

// servedAtAny reports whether an enabled binding of an enabled model r matches is at one of hosts.
func servedAtAny(idx *CatalogIndex, r modelref.Ref, hosts []*host.Host) bool {
	for _, h := range hosts {
		if servedAt(idx, h, func(prov string, m *model.Model) bool { return r.Matches(prov, m.Meta.Name, h.Meta.Name) }) {
			return true
		}
	}
	return false
}

// servedAt reports whether an enabled binding at h serves an enabled model match accepts.
func servedAt(idx *CatalogIndex, h *host.Host, match func(prov string, m *model.Model) bool) bool {
	for _, b := range idx.bindingsByHost[h.Meta.ID] {
		m, ok := idx.Snap.Model(b.Spec.ModelID)
		if !ok || !m.IsEnabled() {
			continue
		}
		if prov, _ := idx.Snap.ProviderSlug(m.Meta.Owner.ID); match(prov, m) {
			return true
		}
	}
	return false
}

// checkPoolHosts files pool_outside_models on each linked pool whose host serves none of the policy's model entries.
func checkPoolHosts(tx *engine.Txn, src engine.Ref, idx *CatalogIndex, entries []modelEntry, pools []linkedPool) {
	for _, lp := range pools {
		h, ok := idx.Snap.Host(lp.pool.Spec.HostID)
		if !ok {
			continue
		}
		if servedAt(idx, h, func(prov string, m *model.Model) bool {
			return slices.ContainsFunc(entries, func(e modelEntry) bool { return e.ref.Matches(prov, m.Meta.Name, h.Meta.Name) })
		}) {
			continue
		}
		tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssuePoolOutsideModels, Source: src, Field: fmt.Sprintf("spec.poolIds[%d]", lp.i), Target: engine.Ref{Kind: KindPool, ID: lp.pool.Meta.ID},
			Message: "pool " + lp.pool.Meta.Name + " is at host " + h.Meta.Name + ", which serves none of this policy's models"})
	}
}
