package settings

import "fmt"

// Governance is the per-resource mutation policy: whether a catalog-managed
// row of a given kind may be edited or deleted through the generic control
// CRUD API. One value type is shared by every governance:<kind> section so
// the shape — and the decision logic in Governs — stays uniform.
//
// These toggles are a speed-bump against accidental mutation, not a wall:
// editing catalog rows is rare but safe (the router self-heals; nothing
// hard-depends on a specific row surviving), so AllowEdit defaults true.
// Deleting is the click-around risk, so AllowDelete defaults false and an
// operator flips it on when they actually mean to prune.
type Governance struct {
	AllowEdit   bool `json:"allowEdit"`
	AllowDelete bool `json:"allowDelete"`
}

func (g *Governance) Validate() error { return nil }

// Op is a mutating operation subject to the governance check.
type Op string

const (
	OpEdit   Op = "edit"
	OpDelete Op = "delete"
)

// Governance section keys. Colon-namespaced so they double as future RBAC
// permission targets (governance:<resource>). Only the catalog-managed kinds
// need a section; purely user-owned kinds fall through to the user tier in
// Governs.
const (
	SectionGovernanceProvider = "governance:provider"
	SectionGovernanceHost     = "governance:host"
	SectionGovernanceModel    = "governance:model"
	SectionGovernancePolicy   = "governance:policy"
)

// GovernanceSections lists every registered governance:<kind> section key.
var GovernanceSections = []string{
	SectionGovernanceProvider,
	SectionGovernanceHost,
	SectionGovernanceModel,
	SectionGovernancePolicy,
}

// governanceSection maps a resource kind (singular) to its section key.
func governanceSection(kind string) string { return "governance:" + kind }

// Owner-kind values Governs reasons about. Mirrors app/meta.OwnerKind; kept
// as local strings so the settings package stays free of the domain types.
const (
	ownerSystem = "system"
	ownerUser   = "user"
)

// Reader is the narrow read surface Governs needs — satisfied by
// *catalog.Catalog. Settings cache reads are lock-free and total (a
// registered section always returns at least its Defaults).
type Reader interface {
	Setting(section string) (any, bool)
}

// MutationError is returned by Governs when an operation is not permitted.
// The control layer maps it to a 403.
type MutationError struct {
	Op        Op
	Kind      string
	OwnerKind string
	Reason    string
}

func (e *MutationError) Error() string { return e.Reason }

// Governs decides whether op may be applied to a row of the given kind with
// the given ownerKind. It is the single source of truth for mutation
// governance; httpapi calls it and maps a non-nil result to 403.
//
// Owner tiers are hardcoded invariants (not operator-toggleable):
//   - system  → never deleted; edited only via limited APIs (the settings
//     API and specialized endpoints), never generic CRUD. Editing or
//     deleting a system row can break the whole router.
//   - user    → always allowed (the row is the caller's). RBAC will later add
//     an owner.id == caller match here.
//   - else (provider/host-owned, i.e. catalog-managed) → consult the kind's
//     governance:<kind> section; absent or unregistered ⇒ the safe default
//     (edit allowed, delete denied).
func Governs(r Reader, op Op, kind, ownerKind string) error {
	switch ownerKind {
	case ownerSystem:
		return &MutationError{Op: op, Kind: kind, OwnerKind: ownerKind,
			Reason: fmt.Sprintf("%s is system-owned: deletion is never permitted and edits go through limited APIs, not generic CRUD", kind)}
	case ownerUser:
		return nil
	}

	g := Governance{AllowEdit: true, AllowDelete: false}
	if v, ok := r.Setting(governanceSection(kind)); ok {
		if gv, ok := v.(*Governance); ok {
			g = *gv
		}
	}

	allowed := (op == OpEdit && g.AllowEdit) || (op == OpDelete && g.AllowDelete)
	if !allowed {
		return &MutationError{Op: op, Kind: kind, OwnerKind: ownerKind,
			Reason: fmt.Sprintf("%s of %s is disabled on this relay; enable it via the governance:%s settings section", op, kind, kind)}
	}
	return nil
}

func init() {
	for _, name := range GovernanceSections {
		Register(Section{
			Name:     name,
			Defaults: func() any { return &Governance{AllowEdit: true, AllowDelete: false} },
			Decode:   decodeAndValidate[Governance, *Governance],
		})
	}
}
