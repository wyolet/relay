// Package routing resolves an inbound inference request to a fully-typed
// RequestPlan that the pipeline can consume. All catalog lookups happen
// here, against the in-memory snapshot. The pipeline itself is ignorant
// of the snapshot.
//
// Resolution flow:
//   1. Model: caller supplies model name (header or body); look it up
//      via snapshot.ModelsByName.
//   2. Policy: comes from the authenticated RelayKey's PolicyID. (No
//      "default route" indirection in the new arch — RelayKey → Policy
//      is direct. Anonymous traffic is served by a separate package.)
//   3. Authorization: model must appear in Policy.Spec.ModelIDs.
//   4. HostBinding: pick one Host from Model.Spec.Hosts the operator has
//      bound. v1 picks the first enabled binding; multi-host failover is
//      a future feature.
//   5. Host: lookup by binding.HostID for BaseURL.
//   6. Keys: Policy.Spec.HostKeyIDs filtered to those whose Owner.ID is
//      the chosen Host (a key authenticates against one host).
//   7. RateLimit: Policy.Spec.RateLimitID, resolved to []pkgratelimit.Rule.
//
// Each lookup is a snapshot.Get — no PG, no I/O. Resolve() is allocation-
// conscious where it matters but not micro-optimised; the hot-path budget
// dominates this.
package routing

import (
	"errors"
	"fmt"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// Errors returned by Resolve. Each maps to a distinct HTTP status in
// the handler; routing keeps them as typed sentinels so handlers can
// errors.Is() rather than parse strings.
var (
	ErrModelNotFound    = errors.New("routing: model not found")
	ErrModelDisabled    = errors.New("routing: model disabled")
	ErrPolicyNotFound   = errors.New("routing: policy not found")
	ErrPolicyDisabled   = errors.New("routing: policy disabled")
	ErrModelNotInPolicy = errors.New("routing: model not allowed by policy")
	ErrNoHostBinding    = errors.New("routing: no enabled host binding for model")
	ErrHostNotFound     = errors.New("routing: host not found")
	ErrNoKeys           = errors.New("routing: no host keys available for this host")
)

// Request carries the inbound resolution inputs.
type Request struct {
	// ModelName is the slug or upstream-name reference the caller asked
	// for (typically from the body's "model" field).
	ModelName string

	// RelayKey is the authenticated key (already validated for auth).
	// Its Spec.PolicyID drives policy selection.
	RelayKey *relaykey.RelayKey
}

// Plan is the fully-resolved input the pipeline consumes. The handler
// converts this to pipeline.Request, dropping fields the pipeline
// doesn't need.
type Plan struct {
	Model       *model.Model
	Policy      *policy.Policy
	HostBinding *model.HostBinding
	Host        *host.Host
	Keys        []*hostkey.HostKey
	Rules       []pkgratelimit.Rule
}

// Resolver wraps a Catalog snapshot accessor and answers Resolve calls.
type Resolver struct {
	cat *appcatalog.Catalog
}

// New constructs a Resolver against the live catalog. The Resolver
// reads cat.Current() on every Resolve — picking up the latest snapshot
// after any NOTIFY-driven reload.
func New(cat *appcatalog.Catalog) *Resolver { return &Resolver{cat: cat} }

// Resolve maps the inbound request to a Plan. Errors are typed; handlers
// pick the appropriate HTTP status.
func (r *Resolver) Resolve(req Request) (*Plan, error) {
	if req.RelayKey == nil {
		return nil, fmt.Errorf("routing.Resolve: relay key is required (anonymous mode is served by a separate package)")
	}
	snap := r.cat.Current()

	// 1. Model
	models := snap.ModelsByName(req.ModelName)
	if len(models) == 0 {
		return nil, ErrModelNotFound
	}
	// ModelsByName returns all models sharing the slug (across providers);
	// for the bound RelayKey's Policy we need the one whose ID is allowed.
	// Walk policy's allowed IDs and intersect.
	pol, ok := snap.Policy(req.RelayKey.Spec.PolicyID)
	if !ok {
		return nil, ErrPolicyNotFound
	}
	if !pol.IsEnabled() {
		return nil, ErrPolicyDisabled
	}

	var chosen *model.Model
	for _, m := range models {
		if !m.IsEnabled() {
			continue
		}
		for _, id := range pol.Spec.ModelIDs {
			if id == m.Meta.ID {
				chosen = m
				break
			}
		}
		if chosen != nil {
			break
		}
	}
	if chosen == nil {
		// Disambiguate: was the model itself disabled vs not in policy?
		anyEnabled := false
		for _, m := range models {
			if m.IsEnabled() {
				anyEnabled = true
				break
			}
		}
		if !anyEnabled {
			return nil, ErrModelDisabled
		}
		return nil, ErrModelNotInPolicy
	}

	// 4. HostBinding — first enabled binding for v1.
	var binding *model.HostBinding
	for i := range chosen.Spec.Hosts {
		hb := &chosen.Spec.Hosts[i]
		if hb.IsEnabled() {
			binding = hb
			break
		}
	}
	if binding == nil {
		return nil, ErrNoHostBinding
	}

	// 5. Host
	h, ok := snap.Host(binding.HostID)
	if !ok {
		return nil, ErrHostNotFound
	}

	// 6. Keys — Policy.HostKeyIDs intersect Owner.ID == host.ID.
	keys := hostKeysForHost(snap, pol, h.Meta.ID)
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}

	// 7. Rules
	var rl *ratelimit.RateLimit
	if pol.Spec.RateLimitID != "" {
		rl, _ = snap.RateLimit(pol.Spec.RateLimitID)
	}
	rules := buildRules(pol, rl)

	return &Plan{
		Model:       chosen,
		Policy:      pol,
		HostBinding: binding,
		Host:        h,
		Keys:        keys,
		Rules:       rules,
	}, nil
}

// hostKeysForHost returns the subset of Policy.Spec.HostKeyIDs whose
// Owner.ID == hostID. Enabled-only; ordered to match Policy's listed
// order (keypool's prioritized algo depends on this).
func hostKeysForHost(snap *appcatalog.Snapshot, pol *policy.Policy, hostID string) []*hostkey.HostKey {
	out := make([]*hostkey.HostKey, 0, len(pol.Spec.HostKeyIDs))
	for _, id := range pol.Spec.HostKeyIDs {
		k, ok := snap.HostKey(id)
		if !ok {
			continue
		}
		if k.Spec.Enabled != nil && !*k.Spec.Enabled {
			continue
		}
		if k.Meta.Owner.Kind != meta.OwnerHost || k.Meta.Owner.ID != hostID {
			continue
		}
		out = append(out, k)
	}
	return out
}

// buildRules translates a Policy + its RateLimit into pkgratelimit.Rules
// the limiter understands. v1 supports the policy-level rate limit; per-
// key rules and system rate limits are deferred to the routing layer
// reaching parity with legacy.
//
// Returns nil when the policy has no rate limit attached.
func buildRules(pol *policy.Policy, rl *ratelimit.RateLimit) []pkgratelimit.Rule {
	if rl == nil {
		return nil
	}
	return ratelimit.Resolve(pol, rl)
}
