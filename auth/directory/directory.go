// Package directory is the identity and tenancy data an auth deployment keeps: users and the external identities they sign in with, groups and their members, teams, projects (each in one team), service accounts (each in one project), roles, and the role bindings that grant them to users, groups, service accounts or every authenticated principal. It validates rows, runs the grant check on every write that hands out a permission, invalidates a user's tokens and sessions when the account changes, and reports each committed write to a Notifier. A provisioning source, such as a SCIM client, writes through the narrower Provisioner instead, which reaches only the users and groups that source created.
//
// Every read and write goes through the Stores interfaces: auth/store/postgres is the ready-made implementation, directory/memstore an in-memory one, and directory/storetest the behaviour any other implementation must share. The package also provides the default rbac.ScopeResolver over the team and project hierarchy.
//
// Out of scope: authenticating callers (auth/authn, auth/session, auth/oidc), authorizing the calls themselves (a product's handler asks auth/rbac before calling in), manifests, and HTTP handlers. Kinds and verbs in role rules are the product's vocabulary, handed in through Config.
package directory

import (
	"context"
	"errors"
	"fmt"

	"github.com/wyolet/relay/auth/principal"
	"github.com/wyolet/relay/auth/rbac"
)

// GrantChecker refuses a grant the caller could not make itself; *rbac.Engine satisfies it.
type GrantChecker interface {
	CheckGrant(ctx context.Context, p *principal.Principal, role rbac.Role, scope rbac.Scope, kinds, verbs []string) error
}

// Config wires a Directory.
type Config struct {
	Stores Stores
	Grants GrantChecker
	// Kinds and Verbs are the product's vocabulary. A role rule must name entries of them or the wildcard, and the grant check expands a wildcard over them. Nil accepts any name and checks a wildcard as itself.
	Kinds []string
	Verbs []string
	// Notifier hears every committed write; nil drops them.
	Notifier Notifier
	// Revoker ends a user's sessions when the account is disabled, deleted or its roles change; nil leaves sessions alone.
	Revoker Revoker
}

// Directory runs the operations. It holds no state beyond its Config and the writer WithWriter set, and is safe for concurrent use when the stores are.
type Directory struct {
	cfg    Config
	writer Writer
}

// New returns a Directory over cfg. Every store and the grant checker are required, so no grant is ever made unchecked.
func New(cfg Config) (*Directory, error) {
	if !cfg.Stores.complete() {
		return nil, errors.New("directory: every store is required")
	}
	if cfg.Grants == nil {
		return nil, errors.New("directory: a grant checker is required")
	}
	return &Directory{cfg: cfg}, nil
}

// Stores returns the stores the directory writes through, for reads.
func (d *Directory) Stores() Stores { return d.cfg.Stores }

func (d *Directory) notify(ctx context.Context, kind Kind, op Op, id string) {
	if d.cfg.Notifier != nil {
		d.cfg.Notifier.Notify(ctx, Change{Kind: kind, Op: op, ID: id})
	}
}

func (d *Directory) revoke(ctx context.Context, userID string) error {
	if d.cfg.Revoker == nil {
		return nil
	}
	if err := d.cfg.Revoker.DestroyUser(ctx, userID); err != nil {
		return fmt.Errorf("directory: end sessions of user %s: %w", userID, err)
	}
	return nil
}

func (d *Directory) checkGrant(ctx context.Context, p *principal.Principal, r *Role, scope rbac.Scope) error {
	return d.cfg.Grants.CheckGrant(ctx, p, r.RBAC(), scope, d.cfg.Kinds, d.cfg.Verbs)
}

// prepareCreate mints the id, derives a missing name from the display name, and refuses an id that is already taken.
func prepareCreate[T any](ctx context.Context, s Store[T], kind string, m *Metadata, meta func(*T) *Metadata) error {
	m.ResourceVersion = ""
	if m.ID == "" {
		m.ID = NewID()
	} else if existing, err := s.Get(ctx, m.ID); err != nil {
		return err
	} else if existing != nil {
		return fmt.Errorf("%w: %s id %q is taken", ErrConflict, kind, m.ID)
	}
	if m.Name != "" {
		return nil
	}
	base := Slugify(m.DisplayName)
	if base == "" {
		return fmt.Errorf("%w: %s: a name or a display name is required", ErrInvalid, kind)
	}
	rows, err := s.List(ctx)
	if err != nil {
		return err
	}
	taken := make(map[string]bool, len(rows))
	for _, r := range rows {
		taken[meta(r).Name] = true
	}
	m.Name = UniqueName(base, func(n string) bool { return taken[n] })
	return nil
}

// loadForUpdate returns the stored row and fills a name the update left empty.
func loadForUpdate[T any](ctx context.Context, s Store[T], kind string, m *Metadata, meta func(*T) *Metadata) (*T, error) {
	if m.ID == "" {
		return nil, fmt.Errorf("%w: %s: id is required", ErrInvalid, kind)
	}
	stored, err := s.Get(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	if stored == nil {
		return nil, fmt.Errorf("%w: %s %s", ErrNotFound, kind, m.ID)
	}
	if m.Name == "" {
		m.Name = meta(stored).Name
	}
	return stored, nil
}

// deleteRow refuses a missing id so a caller can answer not found, then deletes and reports it.
func deleteRow[T any](ctx context.Context, d *Directory, s Store[T], kind Kind, id string) error {
	stored, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if stored == nil {
		return fmt.Errorf("%w: %s %s", ErrNotFound, kind, id)
	}
	if err := s.Delete(ctx, id); err != nil {
		return err
	}
	d.notify(ctx, kind, OpDelete, id)
	return nil
}

func defaultOwner(m *Metadata) {
	if m.Owner.Kind == "" {
		m.Owner.Kind = OwnerSystem
	}
}
