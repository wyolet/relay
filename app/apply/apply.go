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
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
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
}

// Counts summarises a plan.
type Counts struct {
	Create    int `json:"create"`
	Update    int `json:"update"`
	Unchanged int `json:"unchanged"`
	SkipDirty int `json:"skipDirty"`
	Delete    int `json:"delete"`
}

// Result is the full ordered set of entries one apply would run — the plan.
// (The verb Plan is the function that builds it.)
type Result struct {
	Entries []Entry `json:"entries"`
	Counts  Counts  `json:"counts"`
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

	b := &builder{opts: opts, rows: rows, idx: newIndex(rows), selector: sel}
	if err := b.run(docs); err != nil {
		return nil, err
	}

	p := &Result{Entries: b.entries}
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
		}
	}
	return p, nil
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
	if authzr != nil {
		for _, e := range p.Entries {
			if e.write == nil {
				continue
			}
			owner := e.owner
			res := authz.Resource{Kind: singularOf(e.plural), ID: e.ID, Name: e.Name, Owner: &owner}
			if err := authzr.Authorize(ctx, e.plural+"."+string(verbOf(e.Action)), res); err != nil {
				return nil, &AuthzError{Entry: e, Err: err}
			}
		}
	}

	applied := make([]Entry, 0, len(p.Entries))
	for _, e := range p.Entries {
		if e.write == nil {
			continue
		}
		if err := e.write(ctx); err != nil {
			return applied, &StoreError{Entry: e, Applied: applied, Err: err}
		}
		applied = append(applied, e)
	}
	return applied, nil
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
