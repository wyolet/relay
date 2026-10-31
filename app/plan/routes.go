package plan

import (
	"hash/maphash"
	"sync"
	"sync/atomic"

	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pricing"
)

// Route-table bounds: shards keep fills of different refs from waiting on each other, and the per-shard cap bounds what unknown model names can fill.
const (
	routeShards    = 16
	routesPerShard = 256
)

// RouteTable maps a caller's model ref to its Route, filling on first use from the catalog index and pools its CallerPlan holds. A table is never filled against newer inputs: any change replaces the CallerPlan and with it the table.
type RouteTable struct {
	caller *CallerPlan
	idx    *CatalogIndex
	seed   maphash.Seed
	shards [routeShards]routeShard
	misses atomic.Uint64
}

// routeShard allocates on its first fill: most principals touch a few models, and a table exists per principal.
type routeShard struct {
	mu     sync.RWMutex
	routes map[string]*Route
	order  []string // ring of filled refs, oldest dropped first
	next   int
}

func newRouteTable(cp *CallerPlan, idx *CatalogIndex) *RouteTable {
	return &RouteTable{caller: cp, idx: idx, seed: maphash.MakeSeed()}
}

// Route is where one model ref goes for one caller: the model, and the hops (pool by pool, the failover order) with the slots that may serve it. Refused is set when no hop remains, naming what emptied the route.
type Route struct {
	Model    *model.Model
	Snapshot *model.Snapshot
	Provider string
	// UpstreamModel is the wire model name: the alias's override when one matched, else the snapshot's upstream name. ResolvedVia tags an alias match for usage rows.
	UpstreamModel string
	ResolvedVia   string
	Hops          []RouteHop
	Refused       *Refusal
}

// RouteHop is one pool's slots that may serve the route, with the binding and pricing at the pool's host. Counters[i] is everything slot i's request must fit: the caller rungs' counters, then the slot's upstream ones.
type RouteHop struct {
	Pool     *PoolPlan
	Binding  *binding.Binding
	Host     *host.Host
	Pricing  *pricing.Pricing
	Priced   bool
	Slots    []*KeySlot
	Counters [][]limits.Counter
}

// Get returns the route for ref, filling it on first use; never nil. A hit is one read-locked map lookup.
func (t *RouteTable) Get(ref string) *Route {
	sh := &t.shards[maphash.String(t.seed, ref)%routeShards]
	sh.mu.RLock()
	r := sh.routes[ref]
	sh.mu.RUnlock()
	if r != nil {
		return r
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if r = sh.routes[ref]; r != nil {
		return r
	}
	t.misses.Add(1)
	r = t.fill(ref)
	if sh.routes == nil {
		sh.routes = make(map[string]*Route)
		sh.order = make([]string, routesPerShard)
	}
	if old := sh.order[sh.next]; old != "" {
		delete(sh.routes, old)
	}
	sh.order[sh.next] = ref
	sh.next = (sh.next + 1) % routesPerShard
	sh.routes[ref] = r
	return r
}

// Misses counts fills, for a gauge.
func (t *RouteTable) Misses() uint64 { return t.misses.Load() }

// Models lists the models the caller can reach, by running the route fill per model: the listing cannot advertise what a request would be refused.
func (t *RouteTable) Models() []*model.Model {
	if t.caller.Refused != nil || t.idx == nil {
		return nil
	}
	var out []*model.Model
	for _, m := range t.idx.Snap.AllModels() {
		if r := t.route([]*model.Model{m}, nil, ""); len(r.Hops) > 0 {
			out = append(out, m)
		}
	}
	return out
}

func (t *RouteTable) fill(ref string) *Route {
	if t.caller.Refused != nil {
		return &Route{Refused: t.caller.Refused}
	}
	if t.idx == nil {
		return &Route{Refused: &Refusal{Code: CodeModelNotFound, Message: "no model named " + ref}}
	}
	res := t.idx.Snap.ResolveModelRef(ref)
	if len(res.Models) == 0 {
		return &Route{Refused: &Refusal{Code: CodeModelNotFound, Message: "no model named " + ref}}
	}
	r := t.route(res.Models, res.Snapshot, res.PinHostID)
	r.Snapshot = res.Snapshot
	r.ResolvedVia, r.UpstreamModel = appcatalog.AliasUpstream(res.Alias, ref)
	if r.UpstreamModel == "" && res.Snapshot != nil {
		r.UpstreamModel = res.Snapshot.Upstream()
	}
	return r
}

// Refusal steps, in the order a fill passes them; when no hop remains the refusal from the furthest step reached is the one reported.
const (
	stepModelDisabled = iota
	stepNoBinding
	stepModelNotAllowed
	stepNoCredentials
	stepUpstream
	stepUnpriced
)

type refusalAt struct {
	step int
	r    *Refusal
}

func (b *refusalAt) note(step int, r *Refusal) {
	if b.r == nil || step > b.step {
		b.step, b.r = step, r
	}
}

// served is one binding that can serve the model, at its effective host.
type served struct {
	b       *binding.Binding
	h       *host.Host
	pricing *pricing.Pricing
}

// route walks the candidate models in order and returns the first with a hop. sn narrows bindings to those serving the snapshot; pinHostID to one host.
func (t *RouteTable) route(models []*model.Model, sn *model.Snapshot, pinHostID string) *Route {
	cp, snap := t.caller, t.idx.Snap
	best := refusalAt{r: &Refusal{Code: CodeModelDisabled, Message: "the model is disabled"}}
	for _, m := range models {
		if !m.IsEnabled() {
			continue
		}
		prov, _ := snap.ProviderSlug(m.Meta.Owner.ID)
		var cands []served
		for _, b := range snap.BindingsForModel(m.Meta.ID) {
			if !b.IsEnabled() || (pinHostID != "" && b.Spec.HostID != pinHostID) || (sn != nil && !b.Serves(sn.Name)) {
				continue
			}
			h, ok := snap.Host(b.Spec.HostID)
			if !ok || !h.Routable() {
				continue
			}
			if i := cp.Allow.rejects(prov, m, h.Meta.Name); i >= 0 {
				best.note(stepModelNotAllowed, cp.notAllowed(i))
				continue
			}
			pr, _ := snap.PricingForBinding(b)
			cands = append(cands, served{b: b, h: h, pricing: pr})
		}
		if len(cands) == 0 {
			best.note(stepNoBinding, &Refusal{Code: CodeNoBinding, Message: "no enabled binding serves " + m.Meta.Name})
			continue
		}
		if hops := t.hops(m, prov, cands, &best); len(hops) > 0 {
			return &Route{Model: m, Provider: prov, Hops: hops}
		}
	}
	return &Route{Refused: best.r}
}

// hops walks the caller's pools in order and keeps, per pool at a candidate host, the slots whose upstream rungs allow the model.
func (t *RouteTable) hops(m *model.Model, prov string, cands []served, best *refusalAt) []RouteHop {
	cp := t.caller
	var hops []RouteHop
	seen := map[*KeySlot]struct{}{}
	reached := false
	for _, p := range cp.Pools {
		var sv *served
		for i := range cands {
			if cands[i].b.Spec.HostID == p.Host.Meta.ID {
				sv = &cands[i]
				break
			}
		}
		if sv == nil {
			continue
		}
		reached = true
		priced := sv.pricing != nil
		if !priced && cp.budgetAt >= 0 {
			best.note(stepUnpriced, rungRefusal(CodeUnpricedUnderBudget, &cp.Rungs[cp.budgetAt], cp.Rungs[cp.budgetAt].Policy.Policy.Meta.ID, "binding "+sv.b.Meta.Name+" has no pricing and a budget applies"))
			continue
		}
		hop := RouteHop{Pool: p, Binding: sv.b, Host: sv.h, Pricing: sv.pricing, Priced: priced}
		for _, s := range p.Slots {
			if _, dup := seen[s]; dup {
				continue
			}
			if s.Refused != nil {
				best.note(stepUpstream, s.Refused)
				continue
			}
			if i := s.rejects(prov, m, sv.h.Meta.Name); i >= 0 {
				r := &s.Upstream[i]
				best.note(stepUpstream, rungRefusal(CodeModelNotAllowed, r, r.Policy.Policy.Meta.ID, m.Meta.Name+" is not allowed by the "+string(r.Kind)+" policy"))
				continue
			}
			if !priced && s.hasBudget {
				r := &s.Upstream[s.budgetAt]
				best.note(stepUnpriced, rungRefusal(CodeUnpricedUnderBudget, r, r.Policy.Policy.Meta.ID, "binding "+sv.b.Meta.Name+" has no pricing and a budget applies"))
				continue
			}
			seen[s] = struct{}{}
			hop.Slots = append(hop.Slots, s)
			hop.Counters = append(hop.Counters, t.counters(s, prov, m, sv.h.Meta.Name))
		}
		if len(hop.Slots) > 0 {
			hops = append(hops, hop)
		}
	}
	if !reached {
		r := &Refusal{Code: CodeNoCredentials, Message: "no reachable pool serves " + m.Meta.Name}
		if cp.linkedAt >= 0 {
			rg := &cp.Rungs[cp.linkedAt]
			r = rungRefusal(CodeNoCredentials, rg, rg.Policy.Policy.Meta.ID, r.Message)
		}
		best.note(stepNoCredentials, r)
	}
	return hops
}

// counters is the full list one slot's request must fit, built once per route so admission concatenates nothing.
func (t *RouteTable) counters(s *KeySlot, prov string, m *model.Model, hostSlug string) []limits.Counter {
	cp := t.caller
	byModel := s.ByModel[m.Meta.ID]
	out := make([]limits.Counter, 0, len(cp.Counters)+len(s.Whole)+len(byModel))
	out = append(out, cp.Counters...)
	for _, sc := range cp.scoped {
		if sc.scope.matches(prov, m.Meta.Name, hostSlug) {
			out = append(out, sc.counters...)
		}
	}
	out = append(out, s.Whole...)
	return append(out, byModel...)
}

// notAllowed names the caller rung that refused a model.
func (cp *CallerPlan) notAllowed(i int) *Refusal {
	if i == noGrant {
		return &Refusal{Code: CodeModelNotAllowed, Message: "deprecated models need a policy that names them or includes deprecated models"}
	}
	r := &cp.Rungs[i]
	return rungRefusal(CodeModelNotAllowed, r, r.Policy.Policy.Meta.ID, "the model is not allowed by the "+string(r.Kind)+" policy")
}
