// Package apply is the single loader that turns manifest documents into
// Postgres rows. It has two halves: Plan diffs the submitted documents
// against the stored rows and reports what would change; Execute authorizes
// every mutating entry and then writes them, in dependency order.
//
// The boot seed is Plan+Execute over a directory with no authorizer and no
// prune (see app/seed), so a seed and a POST /apply of the same tree produce
// the same rows and the same diff vocabulary.
//
// Deliberately out of scope: transactions across kinds (stores are per-kind
// with their own tx; a mid-way store failure is reported with the partial
// applied list and re-applying converges), field-level ownership beyond the
// dirty flag, and reconcile loops.
package apply

import (
	"context"
	"errors"
	"fmt"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/settings"
)

// Action is what a plan entry would do to one row.
type Action string

const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionUnchanged Action = "unchanged"
	// ActionSkipDirty is an operator-edited row the run refused to
	// overwrite. Reported as drift; nothing is written.
	ActionSkipDirty Action = "skip-dirty"
	ActionDelete    Action = "delete"
	// ActionConflict is a row another writer changed between Plan and
	// Execute. Nothing is written for it; re-planning picks up the change.
	ActionConflict Action = "conflict"
)

// Entry is one planned row change.
type Entry struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
	// Action is what would happen (or happened) to this row.
	Action Action `json:"action"`
	// ChangedFields lists the authorable JSON paths that differ, in the
	// same vocabulary an audit row's change.fields uses.
	ChangedFields []string `json:"changedFields,omitempty"`
	// IDMismatch reports a metadata.id in the document that does not match
	// the row its name resolved to. Ids stay server-owned: the document's
	// id is ignored and the mismatch reported.
	IDMismatch string `json:"idMismatch,omitempty"`

	plural string
	owner  meta.Owner
	write  func(context.Context) error
	// prev is what the row looked like when this entry was planned. Execute
	// re-reads and refuses the write if it no longer matches.
	prev rowState
}

// Counts summarises a plan.
type Counts struct {
	Create    int `json:"create"`
	Update    int `json:"update"`
	Unchanged int `json:"unchanged"`
	SkipDirty int `json:"skipDirty"`
	Delete    int `json:"delete"`
	Conflict  int `json:"conflict"`
}

// Result is the full ordered set of entries one apply would run — the plan.
// (The verb Plan is the function that builds it.)
type Result struct {
	Entries []Entry `json:"entries"`
	Counts  Counts  `json:"counts"`

	// reload re-reads the rows the plan was built from, for Execute's
	// conflict check. Nil (a hand-built Result) skips the re-read.
	reload func(context.Context) (*Rows, error)
}

// Options configures a plan.
type Options struct {
	Stores *Stores

	// Force writes over rows an operator edited via the control API
	// (meta.Dirty), resetting them to the manifest. Default false: dirty
	// rows are reported as skip-dirty and left alone.
	Force bool

	// Prune deletes rows that match Selector and are absent from the
	// submitted set. Requires Selector; never touches system-, provider-,
	// or host-owned rows.
	Prune    bool
	Selector string

	// Gov supplies the governance:<kind> sections every planned mutation is
	// checked against. Nil skips the check — the boot seed runs before the
	// settings cache exists.
	Gov settings.Reader

	// License gates the features a manifest may declare. Nil skips the
	// gate: the loaders below the control API (the boot seed, the CLI's
	// seed subcommand) load an operator's own tree from disk, which is not
	// the surface the licence sells. The control plane always supplies one.
	License license.Checker

	// Authz is the authorizer Execute will use. Plan reads it only to know
	// whether there is one at all: without an authorizer the caller is a
	// loader running as the deployment itself (boot seed, CLI), which may
	// re-parent a row the same way an admin can. The control plane always
	// supplies one, so a scoped caller still cannot chown across scopes.
	Authz authz.Authorizer
}

// ErrSelectorRequired is returned when Prune is set without a selector —
// an unfiltered prune would delete every user row the manifest omits.
var ErrSelectorRequired = errors.New("apply: prune requires a selector")

// AuthzError reports the row that failed authorization. Nothing was written.
type AuthzError struct {
	Entry Entry
	Err   error
}

func (e *AuthzError) Error() string {
	return fmt.Sprintf("apply: %s %q: %v", e.Entry.Kind, e.Entry.Name, e.Err)
}
func (e *AuthzError) Unwrap() error { return e.Err }

// InvalidError reports a document whose translated row failed its kind's
// Validate. Nothing was planned or written.
type InvalidError struct {
	Kind, Name string
	Err        error
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("apply: %s %q: %v", e.Kind, e.Name, e.Err)
}
func (e *InvalidError) Unwrap() error { return e.Err }

// GovernanceError reports a row whose mutation the governance settings
// refuse. Nothing was planned or written.
type GovernanceError struct {
	Kind, Name string
	Err        error
}

func (e *GovernanceError) Error() string {
	return fmt.Sprintf("apply: %s %q: %v", e.Kind, e.Name, e.Err)
}
func (e *GovernanceError) Unwrap() error { return e.Err }

// UnsupportedKindError reports a document apply cannot write. Settings and
// identity rows have their own endpoints; ignoring them would make a bundle
// look applied when half of it was dropped.
type UnsupportedKindError struct{ Kind string }

func (e *UnsupportedKindError) Error() string {
	return fmt.Sprintf("apply: kind %q is not applied through this path", e.Kind)
}

// LicenseError reports a document declaring a feature this deployment is not
// licensed for. Unwraps to license.ErrRequired, which the HTTP layer maps to
// 403 with the license_required body.
type LicenseError struct{ Kind, Name, Feature string }

func (e *LicenseError) Error() string {
	return fmt.Sprintf("apply: %s %q needs the %q feature: %v", e.Kind, e.Name, e.Feature, license.ErrRequired)
}
func (e *LicenseError) Unwrap() error { return license.ErrRequired }

// ReservedNameError reports a document claiming a name relay owns.
type ReservedNameError struct{ Kind, Name string }

func (e *ReservedNameError) Error() string {
	return fmt.Sprintf("apply: %s %q is a built-in name", e.Kind, e.Name)
}

// DuplicateError reports two submitted documents naming the same row. Last
// one wins would silently discard the first.
type DuplicateError struct {
	Kind, Name string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("apply: %s %q is declared twice in the submitted documents", e.Kind, e.Name)
}

// StoreError reports a write that failed part-way through. Applied lists the
// entries that landed before it.
type StoreError struct {
	Entry   Entry
	Applied []Entry
	Err     error
}

func (e *StoreError) Error() string {
	return fmt.Sprintf("apply: write %s %q: %v", e.Entry.Kind, e.Entry.Name, e.Err)
}
func (e *StoreError) Unwrap() error { return e.Err }

// Plan diffs docs against the stored rows and returns what would change.
// It writes nothing.
func Plan(ctx context.Context, docs []manifest.Document, opts Options) (*Result, error) {
	if opts.Stores == nil {
		return nil, fmt.Errorf("apply: Stores is required")
	}
	sel, err := parseSelector(opts.Selector)
	if err != nil {
		return nil, err
	}
	if opts.Prune && len(sel) == 0 {
		return nil, ErrSelectorRequired
	}

	rows, err := Load(ctx, opts.Stores)
	if err != nil {
		return nil, err
	}

	b := &builder{opts: opts, rows: rows, idx: newIndex(rows), selector: sel,
		admin: opts.Authz == nil || authz.IsAdmin(ctx), lic: opts.License}
	if err := b.run(docs); err != nil {
		return nil, err
	}

	stores := opts.Stores
	p := &Result{
		Entries: b.entries,
		reload:  func(ctx context.Context) (*Rows, error) { return Load(ctx, stores) },
	}
	recount(p)
	return p, nil
}

// recount refreshes the summary after the entry list changes.
func recount(p *Result) {
	p.Counts = Counts{}
	for _, e := range p.Entries {
		switch e.Action {
		case ActionCreate:
			p.Counts.Create++
		case ActionUpdate:
			p.Counts.Update++
		case ActionUnchanged:
			p.Counts.Unchanged++
		case ActionSkipDirty:
			p.Counts.SkipDirty++
		case ActionDelete:
			p.Counts.Delete++
		case ActionConflict:
			p.Counts.Conflict++
		}
	}
}

// Execute authorizes every mutating entry up front and only then writes.
// A denied row aborts the whole run with an *AuthzError and no writes
// (all-or-nothing at the authorization step). Writes are sequential in plan
// order; a store failure returns a *StoreError carrying what already landed.
// A nil authorizer skips authorization — the boot seed has no actor.
func Execute(ctx context.Context, p *Result, authzr authz.Authorizer) ([]Entry, error) {
	if p == nil {
		return nil, nil
	}
	if err := Authorize(ctx, p, authzr); err != nil {
		return nil, err
	}

	// Re-read once, immediately before writing: a row someone edited through
	// the control API since Plan ran would otherwise be silently overwritten
	// (and un-dirtied) by a diff computed against its older content.
	var current map[string]map[string]rowState
	if p.reload != nil {
		rows, err := p.reload(ctx)
		if err != nil {
			return nil, err
		}
		current = rowStates(rows)
	}

	applied := make([]Entry, 0, len(p.Entries))
	for i := range p.Entries {
		e := &p.Entries[i]
		if e.write == nil {
			continue
		}
		if current != nil && e.prev != current[e.Kind][e.Name] {
			e.Action = ActionConflict
			e.ChangedFields = nil
			e.write = nil
			continue
		}
		if err := e.write(ctx); err != nil {
			recount(p)
			return applied, &StoreError{Entry: *e, Applied: applied, Err: err}
		}
		applied = append(applied, *e)
	}
	recount(p)
	return applied, nil
}

// Authorize checks every entry against authzr and returns an *AuthzError for
// the first denied write. A nil authorizer skips the pass — the boot seed has
// no actor. Callers that only plan (dry run) run it too, so a caller who may
// write nothing never sees the diff.
//
// Non-writing entries (unchanged, skip-dirty) are authorized under get and
// silently dropped when denied: reporting a row the caller may not read would
// make the plan an oracle for other tenants' state.
func Authorize(ctx context.Context, p *Result, authzr authz.Authorizer) error {
	if p == nil || authzr == nil {
		return nil
	}
	kept := make([]Entry, 0, len(p.Entries))
	for _, e := range p.Entries {
		owner := e.owner
		res := authz.Resource{Kind: singularOf(e.plural), ID: e.ID, Name: e.Name, Owner: &owner}
		if e.write == nil {
			if authzr.Authorize(ctx, e.plural+".get", res) != nil {
				continue
			}
			kept = append(kept, e)
			continue
		}
		if err := authzr.Authorize(ctx, e.plural+"."+string(verbOf(e.Action)), res); err != nil {
			return &AuthzError{Entry: e, Err: err}
		}
		kept = append(kept, e)
	}
	p.Entries = kept
	recount(p)
	return nil
}

// verbOf maps a plan action to the RBAC verb the row is authorized under.
func verbOf(a Action) Action {
	if a == ActionDelete {
		return ActionDelete
	}
	if a == ActionCreate {
		return ActionCreate
	}
	return ActionUpdate
}
