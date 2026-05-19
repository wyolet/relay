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
//   3. Authorization: model must be allowed by the Policy. Allowed if
//      its id is in Spec.ModelIDs, OR Spec.Models (modelref DSL) matches,
//      OR — when both grant fields are empty — the policy is an implicit
//      wildcard: any model reachable via its hostkeys is allowed. The
//      hostkey-coverage check below is the real gate in that case.
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
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/settings"
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

	// ErrPolicyless is returned when a RelayKey has no PolicyID and the
	// inference settings forbid policy-less traffic.
	ErrPolicyless = errors.New("routing: relay key has no policy and policy-less traffic is disabled")
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
//
// Snapshot is the dated checkpoint the caller's model name resolved to:
// either a direct snapshot-name match, or the Model's Pointer when the
// caller used the bare Model name. The handler rewrites the request body's
// `model` field to Snapshot.OriginalName before invoking the adapter.
type Plan struct {
	Model       *model.Model
	Snapshot    *model.Snapshot
	Policy      *policy.Policy
	HostBinding *model.HostBinding
	Host        *host.Host
	Provider    string
	Keys        []*hostkey.HostKey
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

	// 1. Model. Resolution order:
	//    a. Pinned snapshot — exact match on a Spec.Snapshot's Name.
	//    b. Model name (Meta.Name or alias) — follow Spec.Pointer to its snapshot.
	models, snapMatch := resolveModel(snap, req.ModelName)
	if len(models) == 0 {
		return nil, ErrModelNotFound
	}
	// Policy-less flow: when the RelayKey has no attached Policy, the
	// behavior is decided by settings.Inference.AllowMissingPolicy. When
	// allowed, the request bypasses the policy-grant + policy-RL paths
	// and just needs a (model, host) triple the relay has a hostkey for.
	if req.RelayKey.Spec.PolicyID == "" {
		if !r.allowPolicylessTraffic() {
			return nil, ErrPolicyless
		}
		return r.resolvePolicyless(snap, models, snapMatch)
	}

	pol, ok := snap.Policy(req.RelayKey.Spec.PolicyID)
	if !ok {
		return nil, ErrPolicyNotFound
	}
	if !pol.IsEnabled() {
		return nil, ErrPolicyDisabled
	}

	// Pick the (model, binding, host) triple in one pass. A triple is
	// allowed when EITHER:
	//   - The Model's id is in pol.Spec.ModelIDs (legacy literal grant —
	//     binding-agnostic).
	//   - At least one ref in pol.Spec.Models matches
	//     (provider-slug, model-slug, host-slug) per modelref semantics.
	//
	// Walks candidates in declared order so the first-enabled-binding
	// rule still wins when nothing narrows it. anyEnabledModel tracks
	// whether any candidate could have been picked if the policy had
	// allowed it — drives the disabled-vs-not-in-policy diagnosis.
	var (
		chosen        *model.Model
		binding       *model.HostBinding
		chosenHost    *host.Host
		anyEnabledMod bool
		anyEnabledBnd bool
	)
	// A Policy with neither ModelIDs nor Models set is an *implicit
	// wildcard*: it grants every model reachable through the policy's
	// hostkeys. The hostkey-coverage check downstream is the real
	// authorization gate; Spec.Models, when set, narrows that gate. This
	// matches the documented semantics on policy.Spec.Models.
	wildcardGrant := len(pol.Spec.ModelIDs) == 0 && len(pol.Spec.Models) == 0
candidates:
	for _, m := range models {
		if !m.IsEnabled() {
			continue
		}
		anyEnabledMod = true
		providerSlug, _ := snap.ProviderSlug(m.Meta.Owner.ID)
		legacyAllowed := false
		for _, id := range pol.Spec.ModelIDs {
			if id == m.Meta.ID {
				legacyAllowed = true
				break
			}
		}
		deprecated := isDeprecated(m)
		for i := range m.Spec.Hosts {
			hb := &m.Spec.Hosts[i]
			if !hb.IsEnabled() {
				continue
			}
			h, ok := snap.Host(hb.HostID)
			if !ok {
				continue
			}
			anyEnabledBnd = true
			// Allow paths in priority order:
			//   1. Legacy literal ModelIDs grant — always allowed.
			//   2. Modelref match in Spec.Models — wildcard refs hide
			//      deprecated models unless IncludeDeprecated.
			//   3. Implicit wildcard (both grant fields empty) — same
			//      deprecated-hide rule as a top-level wildcard ref.
			allowed := legacyAllowed
			if !allowed {
				switch {
				case len(pol.Spec.Models) > 0:
					allowed = refsAllow(pol.Spec.Models, providerSlug, m.Meta.Name, h.Meta.Name, deprecated && !pol.Spec.IncludeDeprecated)
				case wildcardGrant:
					allowed = !deprecated || pol.Spec.IncludeDeprecated
				}
			}
			if !allowed {
				continue
			}
			chosen = m
			binding = hb
			chosenHost = h
			break candidates
		}
	}
	if chosen == nil {
		if !anyEnabledMod {
			return nil, ErrModelDisabled
		}
		if !anyEnabledBnd {
			return nil, ErrNoHostBinding
		}
		return nil, ErrModelNotInPolicy
	}
	h := chosenHost

	// 6. Keys — Policy.HostKeyIDs intersect Owner.ID == host.ID.
	keys := hostKeysForHost(snap, pol, h.Meta.ID)
	if len(keys) == 0 {
		return nil, ErrNoKeys
	}

	providerSlug, _ := snap.ProviderSlug(chosen.Meta.Owner.ID)

	return &Plan{
		Model:       chosen,
		Snapshot:    pickSnapshot(chosen, snapMatch),
		Policy:      pol,
		HostBinding: binding,
		Host:        h,
		Provider:    providerSlug,
		Keys:        keys,
	}, nil
}

// resolveModel implements the v1alpha2 lookup order:
//  1. If req.ModelName matches a Spec.Snapshot's Name, return the owning
//     Model + that snapshot.
//  2. Otherwise look up by Model name (Meta.Name or alias). When the
//     winning Model has snapshots, return the one named by Spec.Pointer.
//
// snapMatch is non-nil only when the caller pinned a specific snapshot
// name; downstream code may use it to skip pointer resolution per candidate.
func resolveModel(snap *appcatalog.Snapshot, name string) ([]*model.Model, *model.Snapshot) {
	if m, s, ok := snap.SnapshotByName(name); ok {
		return []*model.Model{m}, s
	}
	return snap.ModelsByName(name), nil
}

// pickSnapshot returns the snapshot for m: the pinned one if non-nil,
// otherwise the snapshot named by m.Spec.Pointer. Returns nil if no
// snapshot can be selected (catalog inconsistency — validation should
// prevent this).
func pickSnapshot(m *model.Model, pinned *model.Snapshot) *model.Snapshot {
	if pinned != nil {
		return pinned
	}
	for i := range m.Spec.Snapshots {
		if m.Spec.Snapshots[i].Name == m.Spec.Pointer {
			return &m.Spec.Snapshots[i]
		}
	}
	return nil
}

// hostKeysForHost returns the subset of Policy.Spec.HostKeyIDs whose
// Owner.ID == hostID. Enabled-only; ordered to match Policy's listed
// order (keypool's prioritized algo depends on this).
// refsAllow reports whether any of the policy's ref strings matches
// the candidate (provider, model, host) triple. Refs that fail to parse
// are skipped silently — Validate rejects them at write time, so a bad
// ref reaching here means a stored row was hand-edited; ignoring is
// safer than erroring the request.
//
// hideForDeprecated, when true, requires the ref to name the model
// explicitly (ref.Model == modelSlug, not a wildcard). Wildcard matches
// — provider, provider@host, @host — are rejected for deprecated models
// unless the policy opted in via IncludeDeprecated. The reasoning:
// "anthropic" should not silently grant access to last year's sunset
// model; "anthropic/claude-3-haiku-20240307" obviously means to.
func refsAllow(refs []string, providerSlug, modelSlug, hostSlug string, hideForDeprecated bool) bool {
	for _, raw := range refs {
		ref, err := modelref.Parse(raw)
		if err != nil {
			continue
		}
		if !ref.Matches(providerSlug, modelSlug, hostSlug) {
			continue
		}
		if hideForDeprecated && ref.ModelWildcard {
			continue
		}
		return true
	}
	return false
}

// isDeprecated reports whether m's lifecycle status excludes it from
// wildcard grants by default. Both "deprecated" and "sunset" qualify;
// "active" (or unset) does not.
func isDeprecated(m *model.Model) bool {
	if m.Spec.Deprecation == nil {
		return false
	}
	switch m.Spec.Deprecation.Status {
	case model.DeprecationDeprecated, model.DeprecationSunset:
		return true
	}
	return false
}

// allowPolicylessTraffic reads settings.Inference.AllowMissingPolicy.
// Missing or malformed setting → false (closed default).
func (r *Resolver) allowPolicylessTraffic() bool {
	v, ok := r.cat.Setting(settings.SectionInference)
	if !ok {
		return false
	}
	cfg, ok := v.(*settings.Inference)
	if !ok || cfg == nil {
		return false
	}
	return cfg.AllowMissingPolicy
}

// resolvePolicyless picks the first (model, binding, host) triple where
// the relay has any enabled hostkey for the host. No policy filter, no
// policy-level rate limits — Plan.Policy is nil, Plan.Keys is the full
// pool of hostkeys for the chosen host.
func (r *Resolver) resolvePolicyless(snap *appcatalog.Snapshot, models []*model.Model, snapMatch *model.Snapshot) (*Plan, error) {
	var (
		anyEnabledMod bool
		anyEnabledBnd bool
	)
	for _, m := range models {
		if !m.IsEnabled() {
			continue
		}
		anyEnabledMod = true
		// Skip deprecated models by default for the same reason wildcard
		// grants do — the operator would explicitly grant a sunset model
		// by configuring a Policy if they meant to.
		if isDeprecated(m) {
			continue
		}
		for i := range m.Spec.Hosts {
			hb := &m.Spec.Hosts[i]
			if !hb.IsEnabled() {
				continue
			}
			h, ok := snap.Host(hb.HostID)
			if !ok {
				continue
			}
			anyEnabledBnd = true
			keys := snap.HostKeysForHost(h.Meta.ID)
			if len(keys) == 0 {
				continue
			}
			providerSlug, _ := snap.ProviderSlug(m.Meta.Owner.ID)
			return &Plan{
				Model:       m,
				Snapshot:    pickSnapshot(m, snapMatch),
				Policy:      nil,
				HostBinding: hb,
				Host:        h,
				Provider:    providerSlug,
				Keys:        keys,
			}, nil
		}
	}
	if !anyEnabledMod {
		return nil, ErrModelDisabled
	}
	if !anyEnabledBnd {
		return nil, ErrNoHostBinding
	}
	return nil, ErrNoKeys
}

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
		if k.Spec.HostID != hostID {
			continue
		}
		out = append(out, k)
	}
	return out
}

