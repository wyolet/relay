package manifest

import (
	"context"
	"errors"
	"fmt"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/principal"
)

// Action is what an entry does to one row.
type Action string

const (
	ActionCreate    Action = "create"
	ActionUpdate    Action = "update"
	ActionUnchanged Action = "unchanged"
	// ActionSkipDirty is a row an operator edited after a manifest wrote it; it is left alone unless Options.Force is set.
	ActionSkipDirty Action = "skip-dirty"
	ActionDelete    Action = "delete"
)

// Entry is one planned row change.
type Entry struct {
	Kind   string
	Name   string
	ID     string
	Action Action
	// ChangedFields lists the document paths that differ ("spec.team", "metadata.labels").
	ChangedFields []string
	// IDMismatch is a metadata.id the document gave that is not the id its name resolved to; the document's id is ignored.
	IDMismatch string

	write func(ctx context.Context, dir *directory.Directory, caller *principal.Principal) error
}

// Plan is the ordered set of entries one Apply runs: creates and updates in dependency order, then deletes in reverse.
type Plan struct {
	Entries []Entry
}

// Options configures Plan.
type Options struct {
	// Force writes over rows marked Dirty.
	Force bool
	// Prune deletes stored rows that carry every Selector label and are not in the documents. Users are never pruned: an account carries no labels to say a manifest owns it.
	Prune    bool
	Selector map[string]string
	// Refs resolves the product's own kinds that product fields name; Names is its reverse, for the diff.
	Refs  Resolver
	Names Namer
}

// ErrSelectorRequired reports Prune without a Selector, which would delete every row the documents leave out, rows created by hand included.
var ErrSelectorRequired = errors.New("manifest: prune requires a selector")

// ApplyError reports the entry whose write failed. Applied lists the entries written before it.
type ApplyError struct {
	Entry   Entry
	Applied []Entry
	Err     error
}

func (e *ApplyError) Error() string {
	return fmt.Sprintf("manifest: %s %s %q: %v", e.Entry.Action, e.Entry.Kind, e.Entry.Name, e.Err)
}
func (e *ApplyError) Unwrap() error { return e.Err }

// Plan diffs docs against the rows in stores and returns what Apply would write. It writes nothing.
func (r *Registry) Plan(ctx context.Context, stores directory.Stores, docs []Document, opts Options) (*Plan, error) {
	if opts.Prune && len(opts.Selector) == 0 {
		return nil, ErrSelectorRequired
	}
	rows, err := loadRows(ctx, stores)
	if err != nil {
		return nil, err
	}
	byKind, err := groupDocs(docs)
	if err != nil {
		return nil, err
	}
	idx := newIndex(rows, opts)
	idx.mint(byKind)
	p := &planner{reg: r, opts: opts, idx: idx, rows: rows, docs: byKind}
	if err := p.run(); err != nil {
		return nil, err
	}
	return &Plan{Entries: p.entries}, nil
}

// Apply runs every write in order, as caller, through dir. A failed write stops the run with an *ApplyError; re-planning picks up from what landed.
func (p *Plan) Apply(ctx context.Context, dir *directory.Directory, caller *principal.Principal) ([]Entry, error) {
	applied := make([]Entry, 0, len(p.Entries))
	for _, e := range p.Entries {
		if e.write == nil {
			continue
		}
		if err := e.write(ctx, dir, caller); err != nil {
			return applied, &ApplyError{Entry: e, Applied: applied, Err: err}
		}
		applied = append(applied, e)
	}
	return applied, nil
}

// groupDocs buckets documents by kind and refuses a name declared twice in one kind: the later document would silently replace the earlier.
func groupDocs(docs []Document) (map[string][]*Document, error) {
	out := map[string][]*Document{}
	seen := map[string]bool{}
	for i := range docs {
		d := &docs[i]
		key := d.Kind + "/" + d.Metadata.Name
		if seen[key] {
			return nil, &Error{Doc: d.Index, Kind: d.Kind, Name: d.Metadata.Name, Err: errors.New("declared twice")}
		}
		seen[key] = true
		out[d.Kind] = append(out[d.Kind], d)
	}
	return out, nil
}

// rows is every stored row, read once.
type rows struct {
	teams           []*directory.Team
	projects        []*directory.Project
	serviceAccounts []*directory.ServiceAccount
	roles           []*directory.Role
	users           []*directory.User
	groups          []*directory.Group
	bindings        []*directory.RoleBinding
}

func loadRows(ctx context.Context, s directory.Stores) (*rows, error) {
	var out rows
	var err error
	if out.teams, err = s.Teams.List(ctx); err != nil {
		return nil, fmt.Errorf("manifest: list teams: %w", err)
	}
	if out.projects, err = s.Projects.List(ctx); err != nil {
		return nil, fmt.Errorf("manifest: list projects: %w", err)
	}
	if out.serviceAccounts, err = s.ServiceAccounts.List(ctx); err != nil {
		return nil, fmt.Errorf("manifest: list service accounts: %w", err)
	}
	if out.roles, err = s.Roles.List(ctx); err != nil {
		return nil, fmt.Errorf("manifest: list roles: %w", err)
	}
	if out.users, err = s.Users.List(ctx); err != nil {
		return nil, fmt.Errorf("manifest: list users: %w", err)
	}
	if out.groups, err = s.Groups.List(ctx); err != nil {
		return nil, fmt.Errorf("manifest: list groups: %w", err)
	}
	if out.bindings, err = s.RoleBindings.List(ctx); err != nil {
		return nil, fmt.Errorf("manifest: list role bindings: %w", err)
	}
	return &out, nil
}

// index maps names to ids and back per kind, over the stored rows plus the ids minted for names the documents introduce, so a reference to a row the same plan creates resolves.
type index struct {
	ids   map[string]map[string]string
	names map[string]map[string]string
	opts  Options
}

func newIndex(r *rows, opts Options) *index {
	idx := &index{ids: map[string]map[string]string{}, names: map[string]map[string]string{}, opts: opts}
	for _, x := range r.teams {
		idx.add(KindTeam, x.Meta.Name, x.Meta.ID)
	}
	for _, x := range r.projects {
		idx.add(KindProject, x.Meta.Name, x.Meta.ID)
	}
	for _, x := range r.serviceAccounts {
		idx.add(KindServiceAccount, x.Meta.Name, x.Meta.ID)
	}
	for _, x := range r.roles {
		idx.add(KindRole, x.Meta.Name, x.Meta.ID)
	}
	for _, x := range r.users {
		idx.add(KindUser, x.Username, x.ID)
	}
	for _, x := range r.groups {
		idx.add(KindGroup, x.Meta.Name, x.Meta.ID)
	}
	for _, x := range r.bindings {
		idx.add(KindRoleBinding, x.Meta.Name, x.Meta.ID)
	}
	return idx
}

func (i *index) add(kind, name, id string) {
	if i.ids[kind] == nil {
		i.ids[kind], i.names[kind] = map[string]string{}, map[string]string{}
	}
	i.ids[kind][name], i.names[kind][id] = id, name
}

func (i *index) mint(byKind map[string][]*Document) {
	for kind, docs := range byKind {
		for _, d := range docs {
			if _, ok := i.ids[kind][d.Metadata.Name]; !ok {
				i.add(kind, d.Metadata.Name, directory.NewID())
			}
		}
	}
}

func (i *index) ID(kind, name string) (string, bool) {
	if IsKind(kind) {
		id, ok := i.ids[kind][name]
		return id, ok
	}
	return lookup(i.opts.Refs, kind, name)
}

func (i *index) Name(kind, id string) (string, bool) {
	if IsKind(kind) {
		n, ok := i.names[kind][id]
		return n, ok
	}
	if i.opts.Names == nil {
		return "", false
	}
	return i.opts.Names.Name(kind, id)
}
