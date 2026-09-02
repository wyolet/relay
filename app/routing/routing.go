// Package routing resolves an inbound inference request to a fully-typed
// RequestPlan that the pipeline can consume. All catalog lookups happen
// here, against the in-memory snapshot. The pipeline itself is ignorant
// of the snapshot.
//
// Resolution flow:
//  1. Model: caller supplies a snapshot name (from the request body's
//     `model` field); look it up via snapshot.SnapshotByName. The owning
//     Model + the picked Snapshot are carried into the Plan.
//  2. Policy: supplied by the caller, already resolved from the
//     credential's principal at the edge. (No "default route"
//     indirection; anonymous traffic is served by a separate package.)
//  3. Authorization: model must be allowed by the Policy. Allowed if
//     its id is in Spec.ModelIDs, OR Spec.Models (modelref DSL) matches,
//     OR — when both grant fields are empty — the policy is an implicit
//     wildcard: any model reachable via its hostkeys is allowed. The
//     hostkey-coverage check below is the real gate in that case.
//  4. HostBinding: pick one of the model's HostBindings (snapshot.
//     BindingsForModel) the operator has configured. v1 picks the first
//     enabled binding; multi-host failover is a future feature.
//  5. Host: lookup by binding.Spec.HostID for BaseURL.
//  6. Keys: Policy.Spec.HostKeyIDs filtered to those whose Owner.ID is
//     the chosen Host (a key authenticates against one host).
//  7. RateLimit: Policy.Spec.RateLimitID, resolved to []pkgratelimit.Rule.
//
// Each lookup is a snapshot.Get — no PG, no I/O. Resolve() is allocation-
// conscious where it matters but not micro-optimised; the hot-path budget
// dominates this.
package routing

import (
	"errors"
	"strings"

	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/pkg/slug"
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

	// ErrPolicyless is returned when a Key has no PolicyID and the
	// inference settings forbid policy-less traffic.
	ErrPolicyless = errors.New("routing: relay key has no policy and policy-less traffic is disabled")
)

// Request carries the inbound resolution inputs.
type Request struct {
	// ModelName is the slug or upstream-name reference the caller asked
	// for (typically from the body's "model" field), possibly with a
	// header-derived "@host" pin folded in by the handler.
	ModelName string

	// RawModelName is the verbatim caller string before any header pin
	// folding. Carried upstream as the wire name when the ref matches a
	// wildcard alias. Falls back to ModelName when empty.
	RawModelName string

	// Policy is the caller's resolved inbound policy (the middleware walks
	// key → service account → policy binding). Nil selects the policy-less
	// flow, which only a project-less personal key can reach.
	Policy *policy.Policy

	// UserID is the calling user, from the credential's principal. It scopes
	// the policy-less pool to that user's own host keys; empty (no user
	// resolved) narrows the pool to system-owned keys.
	UserID string

	// PayloadLoggingEnabled is the caller's own opt-in for body capture;
	// the matched Policy's flag is OR'd onto it.
	PayloadLoggingEnabled bool

	// SkipKeyCheck, when true, suppresses the Policy.HostKeyIDs → host
	// coverage gate. Used by proxy mode: the caller brings their own
	// upstream credentials, so the relay's keypool is irrelevant — only
	// the (model, binding, host) tuple matters. Plan.Keys is nil in
	// this mode.
	SkipKeyCheck bool

	// Snapshot pins the catalog view this resolution reads. Nil takes the
	// live one; handlers pass the snapshot the auth middleware already
	// resolved against, so one request never straddles two of them.
	Snapshot *appcatalog.Snapshot
}

// Plan is the fully-resolved input the pipeline consumes. The handler
// converts this to pipeline.Request, dropping fields the pipeline
// doesn't need.
//
// Snapshot is the resolved checkpoint. The handler rewrites the request
// body's `model` field to Plan.UpstreamModel() before invoking the adapter.
type Plan struct {
	Model       *model.Model
	Snapshot    *model.Snapshot
	Policy      *policy.Policy
	HostBinding *binding.Binding
	Host        *host.Host
	Provider    string
	Keys        []*hostkey.HostKey

	// Pricing is the rate sheet billing against the chosen binding (explicit
	// binding ref first, else the host-owned (model, host) cover). Resolved
	// here so emit-time cost stamping costs zero extra lookups. Nil = unpriced.
	Pricing *pricing.Pricing

	// PayloadLoggingEnabled is the resolved opt-in for full request/response
	// body capture: true when the matched Policy or the inbound Key
	// sets PayloadLoggingEnabled. Read by the inference entry to flag the
	// lifecycle Context so the payloadlog observer captures bodies.
	PayloadLoggingEnabled bool

	// UpstreamOverride, when non-empty, replaces Snapshot.Upstream() as
	// the wire model name. Set only by declared-alias resolution: the
	// alias string itself (exact match) or the caller's raw request
	// string (wildcard match). Consumers read the wire name via
	// UpstreamModel(), never Snapshot.Upstream() directly.
	UpstreamOverride string

	// ResolvedVia tags non-canonical resolution for usage events, e.g.
	// "alias:claude-fable-5[1m]" (the declared alias or pattern that
	// matched). Empty for normal snapshot-name resolution.
	ResolvedVia string
}

// UpstreamModel returns the wire model name for this plan — the alias
// verbatim override when resolution went through a declared alias,
// otherwise the snapshot's upstream name.
func (p *Plan) UpstreamModel() string {
	if p.UpstreamOverride != "" {
		return p.UpstreamOverride
	}
	return p.Snapshot.Upstream()
}

// settingsReader is the narrow settings read the policy-less gate needs;
// *appcatalog.Catalog satisfies it.
type settingsReader interface {
	Setting(section string) (any, bool)
}

// Resolver wraps a Catalog snapshot accessor and answers Resolve calls.
type Resolver struct {
	cat *appcatalog.Catalog
	cfg settingsReader

	// requirePolicy refuses the policy-less flow outright, whatever the
	// inference setting says. See RequirePolicy.
	requirePolicy bool
}

// Option configures a Resolver at composition time.
type Option func(*Resolver)

// RequirePolicy refuses policy-less traffic whatever
// settings.Inference.AllowMissingPolicy says. RBAC authorization wires it:
// there a credential's grants are the whole access model, so a key whose
// policy does not resolve has no access rather than the shared pool's (D82).
func RequirePolicy() Option { return func(r *Resolver) { r.requirePolicy = true } }

// New constructs a Resolver against the live catalog. The Resolver
// reads cat.Current() on every Resolve — picking up the latest snapshot
// after any NOTIFY-driven reload.
func New(cat *appcatalog.Catalog, opts ...Option) *Resolver {
	r := &Resolver{cat: cat, cfg: cat}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Resolve maps the inbound request to a Plan. Errors are typed; handlers
// pick the appropriate HTTP status.
func (r *Resolver) Resolve(req Request) (*Plan, error) {
	snap := req.Snapshot
	if snap == nil {
		snap = r.cat.Current()
	}

	// 1. Snapshot lookup — customer-facing addressing is purely by
	//    snapshot name (with declared aliases as last-priority matchers).
	//    Model.Meta.Name is admin-only.
	models, snapMatch, pinHostID, alias := resolveModel(snap, req.ModelName)
	if len(models) == 0 {
		return nil, ErrModelNotFound
	}
	// Policy-less flow: when the caller resolved no Policy, the behavior is
	// decided by settings.Inference.AllowMissingPolicy. When allowed, the
	// request bypasses the policy-grant + policy-RL paths and just needs a
	// (model, host) triple the relay has a hostkey for.
	if req.Policy == nil {
		if !r.allowPolicylessTraffic() {
			return nil, ErrPolicyless
		}
		plan, err := r.resolvePolicyless(snap, models, snapMatch, pinHostID, req.UserID)
		if err == nil && plan != nil {
			plan.PayloadLoggingEnabled = req.PayloadLoggingEnabled
			applyAlias(plan, alias, req)
		}
		return plan, err
	}

	pol := req.Policy
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
		chosenBnd     *binding.Binding
		chosenHost    *host.Host
		chosenKeys    []*hostkey.HostKey
		anyEnabledMod bool
		anyEnabledBnd bool
		anyAllowed    bool // an allowed candidate existed but had no usable key
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
		deprecated := isDeprecated(m)
		for _, hb := range snap.BindingsForModel(m.Meta.ID) {
			if !hb.IsEnabled() {
				continue
			}
			if pinHostID != "" && hb.Spec.HostID != pinHostID {
				continue
			}
			if snapMatch != nil && !hb.Serves(snapMatch.Name) {
				continue
			}
			h, ok := snap.Host(hb.Spec.HostID)
			if !ok {
				continue
			}
			anyEnabledBnd = true
			// Authorization. An explicit-grant policy carries a precomputed
			// allow-set of (model, host) combos — legacy ModelIDs + Models refs
			// with deprecation already applied (see catalog.policy_allow). An
			// implicit-wildcard policy has no set and allows any non-deprecated
			// model (or all, with IncludeDeprecated).
			var allowed bool
			if wildcardGrant {
				// NoAuth hosts skip the hostkey-coverage gate — the implicit wildcard's only real authz — so reaching one requires an explicit grant.
				allowed = (!deprecated || pol.Spec.IncludeDeprecated) && !h.Spec.NoAuth
			} else {
				allowed = snap.PolicyAllowsCombo(pol.Meta.ID, m.Meta.ID, hb.Spec.HostID)
			}
			if !allowed {
				continue
			}
			anyAllowed = true
			// Keys — Policy.HostKeyIDs intersect Owner.ID == host.ID. A
			// candidate the policy allows but has no usable key for is not the
			// answer: keep walking, since a later binding of the same model may
			// reach a host the policy does hold a key for. Proxy mode
			// (SkipKeyCheck) bypasses the gate; the caller's own upstream
			// credentials replace the keypool.
			if !req.SkipKeyCheck {
				keys := candidateKeys(snap, pol, m, h)
				if len(keys) == 0 {
					continue
				}
				chosenKeys = keys
			}
			chosen = m
			chosenBnd = hb
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
		if anyAllowed {
			return nil, ErrNoKeys
		}
		return nil, ErrModelNotInPolicy
	}
	h := chosenHost
	keys := chosenKeys

	providerSlug, _ := snap.ProviderSlug(chosen.Meta.Owner.ID)
	pr, _ := snap.PricingForBinding(chosenBnd)

	plan := &Plan{
		Model:                 chosen,
		Snapshot:              snapMatch,
		Policy:                pol,
		HostBinding:           chosenBnd,
		Host:                  h,
		Provider:              providerSlug,
		Keys:                  keys,
		Pricing:               pr,
		PayloadLoggingEnabled: pol.Spec.PayloadLoggingEnabled || req.PayloadLoggingEnabled,
	}
	applyAlias(plan, alias, req)
	return plan, nil
}

// applyAlias stamps the verbatim-upstream override and the usage tag when
// the model was matched via a declared alias. Exact aliases carry their
// declared string upstream; wildcard matches carry the caller's raw
// request string (which never holds a header pin — RawModelName is
// captured pre-fold, and in-body pins skip the pattern probe entirely).
func applyAlias(plan *Plan, ref *appcatalog.AliasRef, req Request) {
	if ref == nil {
		return
	}
	plan.ResolvedVia = "alias:" + ref.Name
	if !ref.Pattern {
		plan.UpstreamOverride = ref.Name
		return
	}
	raw := req.RawModelName
	if raw == "" {
		raw = req.ModelName
	}
	plan.UpstreamOverride = raw
}

// resolveModel maps a caller-supplied model ref to its snapshot via the
// snapshot's pre-materialized alias index — a single normalized lookup, no
// request-time parsing. The input is slug-normalized so dotted and slugified
// forms collapse identically ("openai/gpt-5.4-mini" == "openai/gpt-5-4-mini").
// pinHostID is non-empty when the ref named a host ("model@host"), in which
// case binding selection is constrained to that host.
func resolveModel(snap *appcatalog.Snapshot, name string) (models []*model.Model, snap2 *model.Snapshot, pinHostID string, alias *appcatalog.AliasRef) {
	key := slug.From(name)
	if key == "" {
		return nil, nil, "", nil
	}
	if m, s, hostID, ok := snap.ResolveSnapshot(key); ok {
		return []*model.Model{m}, s, hostID, nil
	}
	// Declared aliases are last-priority: probed only after every real
	// catalog name missed. Wildcard patterns are skipped for refs that
	// carry an "@host" pin — the pin segment is glued into the normalized
	// key and would corrupt the match (exact aliases support pins via
	// synthesized forms).
	if ref, ok := snap.ResolveAlias(key, !strings.ContainsRune(name, '@')); ok {
		return []*model.Model{ref.Model}, ref.Snapshot, ref.HostID, &ref
	}
	return nil, nil, "", nil
}

// candidateKeys returns the keys this policy may spend against h for m, or
// nil when the pair is unreachable. A keyless upstream (e.g. self-hosted
// Ollama) gets the synthetic anonymous key so the keypool path is unchanged
// (one candidate, host-scoped breaker) with no real HostKey and no auth
// header; it bypasses the tier gate, having no tier.
func candidateKeys(snap *appcatalog.Snapshot, pol *policy.Policy, m *model.Model, h *host.Host) []*hostkey.HostKey {
	if h.Spec.NoAuth {
		return []*hostkey.HostKey{hostkey.Anonymous(h.Meta.ID, h.Meta.Name)}
	}
	// Tier gate: drop keys whose own (host-owned) policy doesn't grant this
	// (model, host). An implicit-wildcard tier policy allows everything.
	return tierAllowedKeys(snap, hostKeysForHost(snap, pol, h.Meta.ID), m.Meta.ID, h.Meta.ID)
}

// tierAllowedKeys drops keys whose host-owned tier policy doesn't grant the
// (model, host) combination. A key's tier policy (hostkey.Spec.PolicyID)
// defines what that key may serve; an implicit-wildcard tier policy permits
// everything (PolicyAllowsCombo returns true). Reuses the slice backing array
// (hostKeysForHost returns a fresh slice each call).
func tierAllowedKeys(snap *appcatalog.Snapshot, keys []*hostkey.HostKey, modelID, hostID string) []*hostkey.HostKey {
	out := keys[:0]
	for _, k := range keys {
		if snap.PolicyAllowsCombo(k.Spec.PolicyID, modelID, hostID) {
			out = append(out, k)
		}
	}
	return out
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

// PolicylessTrafficAllowed reports whether a caller whose credential
// resolved no policy is served at all. Exported so the /v1/models listing
// asks the same gate resolution applies and cannot advertise what a
// request would be refused. Nil-safe.
func (r *Resolver) PolicylessTrafficAllowed() bool {
	if r == nil {
		return false
	}
	return r.allowPolicylessTraffic()
}

// allowPolicylessTraffic reads settings.Inference.AllowMissingPolicy, which
// only single-user authorization consults — RequirePolicy closes the flow
// ahead of it. Missing or malformed setting → false (closed default).
func (r *Resolver) allowPolicylessTraffic() bool {
	if r.requirePolicy || r.cfg == nil {
		return false
	}
	v, ok := r.cfg.Setting(settings.SectionInference)
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
func (r *Resolver) resolvePolicyless(snap *appcatalog.Snapshot, models []*model.Model, snapMatch *model.Snapshot, pinHostID, userID string) (*Plan, error) {
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
		for _, hb := range snap.BindingsForModel(m.Meta.ID) {
			if !hb.IsEnabled() {
				continue
			}
			if pinHostID != "" && hb.Spec.HostID != pinHostID {
				continue
			}
			if snapMatch != nil && !hb.Serves(snapMatch.Name) {
				continue
			}
			h, ok := snap.Host(hb.Spec.HostID)
			if !ok {
				continue
			}
			anyEnabledBnd = true
			keys := policylessKeys(snap, m, h, userID)
			if len(keys) == 0 {
				continue
			}
			providerSlug, _ := snap.ProviderSlug(m.Meta.Owner.ID)
			pr, _ := snap.PricingForBinding(hb)
			return &Plan{
				Model:       m,
				Snapshot:    snapMatch,
				Policy:      nil,
				HostBinding: hb,
				Host:        h,
				Provider:    providerSlug,
				Keys:        keys,
				Pricing:     pr,
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

// policylessKeys returns the keys a request with no policy may spend against
// h for m, or nil when the pair is unreachable. A keyless upstream gets the
// synthetic anonymous key; everything else draws from the shared pool and
// passes the tier gate. The single definition of the D73 pool: resolution and
// the /v1/models listing both read it, so the two cannot drift.
func policylessKeys(snap *appcatalog.Snapshot, m *model.Model, h *host.Host, userID string) []*hostkey.HostKey {
	if h.Spec.NoAuth {
		return []*hostkey.HostKey{hostkey.Anonymous(h.Meta.ID, h.Meta.Name)}
	}
	return tierAllowedKeys(snap, sharedHostKeys(snap, h.Meta.ID, userID), m.Meta.ID, h.Meta.ID)
}

// sharedHostKeys returns the host's keys a request with no policy may draw
// on: the operator's system-owned ones, plus userID's own. Another user's
// key is their personal credential, and a project's is reachable only
// through that project's policy — spending either here would put the cost
// outside the limits and the attribution that own it. An empty userID
// (no user resolved) leaves the system-owned keys. The result is a fresh
// slice; tierAllowedKeys filters in place.
func sharedHostKeys(snap *appcatalog.Snapshot, hostID, userID string) []*hostkey.HostKey {
	pool := snap.HostKeysForHost(hostID)
	out := make([]*hostkey.HostKey, 0, len(pool))
	for _, k := range pool {
		switch k.Meta.Owner.Kind {
		case meta.OwnerSystem:
			out = append(out, k)
		case meta.OwnerUser:
			if userID != "" && k.Meta.Owner.ID == userID {
				out = append(out, k)
			}
		}
	}
	return out
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
