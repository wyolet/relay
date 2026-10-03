// Package catalog is the composition layer: it pulls the entity stores
// together into an atomic in-memory Snapshot the request path can read in
// O(1). Snapshots are immutable; Reload rebuilds and atomically swaps.
//
// Membership rule (single, simple): every enabled row of every kind enters
// the snapshot. There is no reachability filter — a Model not bound to any
// Policy still appears (it's just unreachable through PoolModels until
// someone wires it up). The "enabled" flag is the entire toggle mechanism.
//
// Reverse joins (modelsByPolicy, pricingByModelHost, etc.) are derived
// indices over the snapshot — they hold pointers to the same rows.
package catalog

import (
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
)

// Snapshot is the immutable in-memory view. All maps are populated at
// construction and never written after — read accessors are safe to call
// from any goroutine.
type Snapshot struct {
	// gen names this view; see nextSnapshotGen.
	gen uint64

	providersByID   map[string]*provider.Provider
	providersByName map[string]*provider.Provider

	hostsByID   map[string]*host.Host
	hostsByName map[string]*host.Host

	policiesByID   map[string]*policy.Policy
	policiesByName map[string]*policy.Policy
	// disabledPoliciesByID holds the rows an operator switched off. They are
	// deliberately out of every index above — nothing routes through them —
	// but a key, service account or policy binding that names one is KEPT,
	// and resolution hands the row back so the request answers 403
	// policy_disabled instead of falling through to a broader grant.
	disabledPoliciesByID map[string]*policy.Policy

	modelsByID map[string]*model.Model
	// modelsByName is multivalued: an alias may legitimately point at more
	// than one Model (e.g. "gpt-5" hosted by both openai and azure). The
	// consumer disambiguates using a suffix in the request string
	// (model@providerSlug) or the X-Relay-Provider header, falling back to
	// the caller's Policy.
	modelsByName map[string][]*model.Model

	// snapshotsByName indexes every Model.Spec.Snapshot's Name → owning
	// Model. Used by request-time resolution to honor pinned snapshot
	// names (e.g. "gpt-4o-2024-11-20") before falling back to the bare
	// Model name and following its Pointer.
	snapshotsByName map[string]snapshotRef

	// snapshotAliases indexes every addressable form of a snapshot, all
	// slug-normalized so request input and stored key collapse identically
	// (e.g. "openai/gpt-5.4-mini" and "openai/gpt-5-4-mini" → one key). Per
	// snapshot we materialize: provider-qualified ("openai/gpt-5-4-mini"),
	// host-pinned ("gpt-5-4-mini@openai"), and both ("openai/gpt-5-4-mini@azure").
	// Host-pinned entries carry the bound HostID so resolution pins that host.
	// Checked AFTER snapshotsByName, so a real snapshot name always wins over
	// a synthesized alias.
	snapshotAliases map[string]snapshotRef

	// aliasExact indexes declared model aliases (model.Spec.Aliases) by
	// every slug-normalized exact form (bare, provider-qualified,
	// host-pinned). Checked only after snapshotsByName + snapshotAliases
	// miss — declared aliases are last-priority matchers and never shadow
	// real catalog names. Cross-model collisions keep a deterministic
	// winner (see insertAliasExact).
	aliasExact map[string]AliasRef

	// aliasPatterns holds the wildcard aliases, sorted longest-prefix
	// first with a deterministic tiebreak. Consulted only on aliasExact
	// miss; see ResolveAlias.
	aliasPatterns []aliasPattern

	// overlaysByTarget mirrors the overlays table: kind|resourceID →
	// the user's sparse patch. Applied to templates at build/reconcile
	// (overlay_apply.go); rows whose target is absent are inert.
	overlaysByTarget map[string]*overlay.Overlay

	// modelTemplates stashes the PRISTINE template for every model whose
	// snapshot entry is an overlay-merged effective row, so reconcile can
	// re-derive (overlay changed) or restore (overlay deleted) without
	// re-reading PG. Sparse: only overlaid models appear.
	modelTemplates map[string]*model.Model

	hostKeysByID map[string]*hostkey.HostKey
	// hostKeysByHost is the per-host pool, sorted by slug, materialized at
	// build/reconcile so the request path takes a slice header instead of
	// scanning and sorting every key in the deployment.
	hostKeysByHost map[string][]*hostkey.HostKey

	rateLimitsByID   map[string]*ratelimit.RateLimit
	rateLimitsByName map[string]*ratelimit.RateLimit

	keysByID map[string]*key.Key
	// keysByHash indexes Spec.KeyHash and, while the key is in its
	// rotation grace window, Spec.PreviousKeyHash.
	keysByHash map[string]*key.Key
	// keysByPrincipal groups keys by "<principal kind>:<id>", so a group or
	// service-account write reindexes that principal's keys instead of
	// walking every key in the deployment.
	keysByPrincipal map[string][]*key.Key
	// subjectsByKey holds each key's precomputed subject list (identity +
	// groups + system groups), so the request path copies a slice header
	// instead of rebuilding it. Recomputed when a group, service account or
	// project write can change it.
	subjectsByKey map[string][]string
	// hashesByUser holds every hash a user's own keys authenticate under,
	// disabled keys included: the usage read scope answers "the rows my own
	// keys produced", and disabling a key does not un-own its past traffic.
	hashesByUser map[string][]string

	// tokenVersionByUser mirrors users.token_version — the only user state
	// the snapshot carries, so token verification stays a map read.
	tokenVersionByUser map[string]int
	// usersLoaded records that tokenVersionByUser came from a users source,
	// so an absent id means a disabled or deleted user rather than "unknown".
	usersLoaded bool

	// Reverse joins precomputed from Policy.Spec.* lists, so the hot path
	// doesn't iterate.
	modelsByPolicy    map[string][]*model.Model
	hostKeysByPolicy  map[string][]*hostkey.HostKey
	rateLimitByPolicy map[string]*ratelimit.RateLimit

	// allowedCombosByPolicy[policyID] is the set of (model, host) pairs an
	// explicit-grant policy allows — built from its ModelIDs + Models refs so
	// authorization is an O(1) membership test. Implicit-wildcard policies are
	// absent (PolicyAllowsCombo returns true). See policy_allow.go.
	allowedCombosByPolicy map[string]map[comboKey]struct{}

	pricingsByID map[string]*pricing.Pricing
	// pricingByModelHost keys on modelID+"|"+hostID for O(1) hot-path lookup.
	pricingByModelHost map[string]*pricing.Pricing

	bindingsByID map[string]*binding.Binding
	// bindingsByModelHost keys on modelID+"|"+hostID for O(1) routing lookup.
	bindingsByModelHost map[string]*binding.Binding
	// bindingsByModel groups a model's bindings (sorted by name) for
	// snapshot-alias generation and per-model enumeration.
	bindingsByModel map[string][]*binding.Binding

	// Reverse-dependency indices: refsByX[X-id] = set of child refKeys that
	// reference this row. Used by the COW reconciler to enumerate dependents
	// when a parent is evicted. Allocated even for empty snapshots so
	// registerRefs has somewhere to write.
	refsByProvider       map[string]refSet
	refsByHost           map[string]refSet
	refsByModel          map[string]refSet
	refsByHostKey        map[string]refSet
	refsByRateLimit      map[string]refSet
	refsByPolicy         map[string]refSet
	refsByTeam           map[string]refSet
	refsByProject        map[string]refSet
	refsByServiceAccount map[string]refSet
	refsByRole           map[string]refSet

	teamsByID   map[string]*team.Team
	teamsByName map[string]*team.Team

	projectsByID   map[string]*project.Project
	projectsByName map[string]*project.Project
	// projectsByTeam groups a team's projects (sorted by name).
	projectsByTeam map[string][]*project.Project

	serviceAccountsByID   map[string]*serviceaccount.ServiceAccount
	serviceAccountsByName map[string]*serviceaccount.ServiceAccount
	// serviceAccountsByProject groups a project's accounts (sorted by name).
	serviceAccountsByProject map[string][]*serviceaccount.ServiceAccount

	groupsByID   map[string]*group.Group
	groupsByName map[string]*group.Group
	// groupsByUser maps a user id to the names of the groups holding them,
	// sorted — the form the subject list needs.
	groupsByUser map[string][]string

	rolesByID   map[string]*role.Role
	rolesByName map[string]*role.Role

	roleBindingsByID map[string]*rolebinding.RoleBinding
	// roleBindingsBySubject keys on the subject string ("user:<id>",
	// "group:<name>", "serviceaccount:<id>"), sorted by binding name.
	roleBindingsBySubject map[string][]*rolebinding.RoleBinding

	policyBindingsByID map[string]*policybinding.PolicyBinding
	// policyBindingsByProject holds a project's bindings, sorted by
	// effective priority then name — the order resolution reads them in.
	policyBindingsByProject map[string][]*policybinding.PolicyBinding

	// now is the clock the time-dependent indices (a rotated key's grace
	// window) read. Nil means wall clock; a test installs its own so a
	// grace transition is reachable without sleeping.
	now func() time.Time
}

// clock returns the snapshot's time source, defaulting to the wall clock.
func (s *Snapshot) clock() time.Time {
	if s == nil || s.now == nil {
		return time.Now()
	}
	return s.now()
}

// snapshotGen numbers every Snapshot ever built or cloned, so a consumer can
// tell one apart from its successor without holding a pointer to it (and
// keeping the whole catalog alive). Starts at 1: the zero value names the
// pre-boot empty snapshot.
var snapshotGen atomic.Uint64

func nextSnapshotGen() uint64 { return snapshotGen.Add(1) }

// Generation identifies this snapshot. Two snapshots are the same view if
// and only if their generations match.
func (s *Snapshot) Generation() uint64 { return s.gen }
