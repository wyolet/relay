package catalog

import (
	"sort"
	"strings"

	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/pkg/slug"
)

// AliasRef is a declared-alias match: the owning Model and the Pointer
// snapshot the alias resolves to. Declared aliases are resolution-only,
// last-priority lookup keys (see model.Spec.Aliases) — routing consults
// them only after ResolveSnapshot misses, so real catalog names always
// shadow them.
type AliasRef struct {
	Model    *model.Model
	Snapshot *model.Snapshot
	// HostID is non-empty only when the matched form carried an "@host"
	// pin (synthesized for exact aliases the same way snapshotAliases
	// pins are).
	HostID string
	// Name is the alias exactly as declared on the model. For exact
	// aliases this is the verbatim upstream wire name; for patterns it
	// identifies the matcher (usage tagging).
	Name string
	// Pattern marks a wildcard match — the upstream wire name is the
	// caller's raw request string, not Name.
	Pattern bool
}

// aliasPattern is one wildcard alias, pre-normalized for boundary-accurate
// matching (slug.FromPrefix keeps the trailing separator dash so
// "claude-fable-5[*]" cannot match "claude-fable-50-foo").
type aliasPattern struct {
	prefix, suffix string
	ref            AliasRef
}

// indexModelAliases materializes a model's declared aliases. Exact aliases
// get the same synthesized forms as snapshot names (bare, provider-
// qualified, host-pinned); patterns are indexed bare-only — a pinned or
// qualified ref never matches a pattern (documented v1 limitation).
// Called from indexModelSnapshots so build/reconcile ordering is shared.
func (s *Snapshot) indexModelAliases(m *model.Model, provSlug string) {
	if len(m.Spec.Aliases) == 0 {
		return
	}
	tgt := m.PointerSnapshot()
	if tgt == nil {
		return // can't happen post-Validate; reconcile stays defensive
	}
	for _, a := range m.Spec.Aliases {
		if prefix, suffix, isPattern := model.AliasPattern(a); isPattern {
			s.aliasPatterns = append(s.aliasPatterns, aliasPattern{
				prefix: prefix,
				suffix: suffix,
				ref:    AliasRef{Model: m, Snapshot: tgt, Name: a, Pattern: true},
			})
			continue
		}
		base := AliasRef{Model: m, Snapshot: tgt, Name: a}
		s.insertAliasExact(slug.From(a), base)
		if provSlug != "" {
			s.insertAliasExact(slug.From(provSlug+"/"+a), base)
		}
		for _, hb := range s.BindingsForModel(m.Meta.ID) {
			if !hb.IsEnabled() {
				continue
			}
			h, ok := s.hostsByID[hb.Spec.HostID]
			if !ok {
				continue
			}
			if _, skip := hostPinSkip[h.Meta.Name]; skip {
				continue
			}
			pinned := AliasRef{Model: m, Snapshot: tgt, HostID: hb.Spec.HostID, Name: a}
			s.insertAliasExact(slug.From(a+"@"+h.Meta.Name), pinned)
			if provSlug != "" {
				s.insertAliasExact(slug.From(provSlug+"/"+a+"@"+h.Meta.Name), pinned)
			}
		}
	}
	s.sortAliasPatterns()
}

// insertAliasExact writes ref under key unless an entry from another model
// already holds it and wins the deterministic tiebreak. Cross-model alias
// collisions are tolerated the same way multivalued modelsByName overlap
// is; the winner is stable across rebuild/reconcile iteration order.
func (s *Snapshot) insertAliasExact(key string, ref AliasRef) {
	if key == "" {
		return
	}
	if cur, ok := s.aliasExact[key]; ok && aliasOrder(cur, ref) <= 0 {
		return
	}
	s.aliasExact[key] = ref
}

// aliasOrder is the deterministic collision tiebreak: lexicographic
// (model slug, declared alias).
func aliasOrder(a, b AliasRef) int {
	if c := strings.Compare(a.Model.Meta.Name, b.Model.Meta.Name); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// sortAliasPatterns keeps overlapping patterns deterministic: longest
// normalized prefix wins, then lexicographic (prefix, suffix, owner).
func (s *Snapshot) sortAliasPatterns() {
	sort.SliceStable(s.aliasPatterns, func(i, j int) bool {
		a, b := s.aliasPatterns[i], s.aliasPatterns[j]
		if len(a.prefix) != len(b.prefix) {
			return len(a.prefix) > len(b.prefix)
		}
		if a.prefix != b.prefix {
			return a.prefix < b.prefix
		}
		if a.suffix != b.suffix {
			return a.suffix < b.suffix
		}
		return aliasOrder(a.ref, b.ref) < 0
	})
}

// deindexModelAliases removes a model's declared aliases from both
// structures, sweeping by owning-model id (robust even when the model's
// hosts were already evicted, mirroring deindexModelSnapshots).
func (s *Snapshot) deindexModelAliases(m *model.Model) {
	for k, ref := range s.aliasExact {
		if ref.Model.Meta.ID == m.Meta.ID {
			delete(s.aliasExact, k)
		}
	}
	if len(s.aliasPatterns) == 0 {
		return
	}
	kept := s.aliasPatterns[:0]
	for _, p := range s.aliasPatterns {
		if p.ref.Model.Meta.ID != m.Meta.ID {
			kept = append(kept, p)
		}
	}
	s.aliasPatterns = kept
}

// ResolveAlias maps a slug-normalized ref to a declared model alias:
// exact map probe first, then — when patterns is true — the wildcard
// list in longest-prefix order. Routing calls this only on
// ResolveSnapshot miss (exact catalog names always win) and passes
// patterns=false for refs that carry an "@host" pin, because the pin
// segment is glued into the normalized key and would corrupt a wildcard
// match.
func (s *Snapshot) ResolveAlias(key string, patterns bool) (AliasRef, bool) {
	if r, ok := s.aliasExact[key]; ok {
		return r, true
	}
	if patterns {
		for _, p := range s.aliasPatterns {
			if len(key) >= len(p.prefix)+len(p.suffix) &&
				strings.HasPrefix(key, p.prefix) && strings.HasSuffix(key, p.suffix) {
				return p.ref, true
			}
		}
	}
	return AliasRef{}, false
}
