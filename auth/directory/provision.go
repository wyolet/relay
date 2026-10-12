package directory

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrForeign reports a provisioning source reaching for a row it did not provision: a local row, or another source's. It always comes wrapped together with ErrNotFound, so the source cannot tell a foreign row from a missing one.
var ErrForeign = errors.New("directory: row belongs to another source")

// Provisioner is the directory as one provisioning source, such as a SCIM client, sees it. It reads and writes only the users and groups stamped with its source, never sets roles, passwords or sign-in identities, and has no operation on roles, bindings, teams, projects or service accounts. Membership of its own groups skips the grant check: the source decides who is in the groups it manages, and the bindings an operator makes on those groups decide what membership grants.
type Provisioner struct {
	d      *Directory
	source string
}

// Provisioner returns the directory as source sees it. Every row it creates is stamped with source, so two clients sharing a source share their rows.
func (d *Directory) Provisioner(source string) (*Provisioner, error) {
	if strings.TrimSpace(source) == "" {
		return nil, fmt.Errorf("%w: a provisioning source needs a name", ErrInvalid)
	}
	return &Provisioner{d: d, source: source}, nil
}

// Source is the source the provisioner writes as.
func (p *Provisioner) Source() string { return p.source }

// ProvisionedUser is what a source may set on an account.
type ProvisionedUser struct {
	Username string
	Email    string
	// ExternalID is the source's own id for the account; empty for none.
	ExternalID string
}

func (p *Provisioner) owns(e External) bool { return e.Source == p.source }

func foreign(kind, id string) error {
	return fmt.Errorf("%w: %w: %s %s", ErrNotFound, ErrForeign, kind, id)
}

// User returns one of the source's accounts; a missing or foreign one is ErrNotFound.
func (p *Provisioner) User(ctx context.Context, id string) (*User, error) { return p.ownUser(ctx, id) }

// Users returns every account the source provisioned, oldest first.
func (p *Provisioner) Users(ctx context.Context) ([]*User, error) {
	all, err := p.d.cfg.Stores.Users.List(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, u := range all {
		if p.owns(u.External) {
			out = append(out, u)
		}
	}
	return out, nil
}

// ownUser returns the stored account, refusing a missing or foreign one.
func (p *Provisioner) ownUser(ctx context.Context, id string) (*User, error) {
	u, err := p.d.cfg.Stores.Users.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("%w: user %s", ErrNotFound, id)
	}
	if !p.owns(u.External) {
		return nil, foreign("user", id)
	}
	return u, nil
}

// CreateUser adds an account stamped with the source, disabled when disabled is set. It holds no roles, password or identity.
func (p *Provisioner) CreateUser(ctx context.Context, in ProvisionedUser, disabled bool) (*User, error) {
	u := &User{
		Username: in.Username, Email: in.Email, Disabled: disabled,
		External: External{Source: p.source, ID: in.ExternalID},
	}
	if err := p.d.CreateUser(ctx, nil, u); err != nil {
		return nil, err
	}
	return p.ownUser(ctx, u.ID)
}

// UpdateUser sets the fields a source owns on one of its accounts and writes nothing else, so roles, password, identities and the disabled flag stay as stored even when an operator changes them while the source's request runs.
func (p *Provisioner) UpdateUser(ctx context.Context, id string, in ProvisionedUser) (*User, error) {
	u, err := p.ownUser(ctx, id)
	if err != nil {
		return nil, err
	}
	next := *u
	next.Username, next.Email, next.External.ID = in.Username, in.Email, in.ExternalID
	if err := next.validate(); err != nil {
		return nil, err
	}
	if err := p.d.checkUserUnique(ctx, &next); err != nil {
		return nil, err
	}
	if err := p.d.cfg.Stores.Users.UpdateProvisioned(ctx, p.source, id, in); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, foreign("user", id)
		}
		return nil, err
	}
	p.d.notify(ctx, KindUser, OpUpsert, id)
	return p.ownUser(ctx, id)
}

// DisableUser disables one of the source's accounts, as Directory.DisableUser does: its sessions end and its tokens die.
func (p *Provisioner) DisableUser(ctx context.Context, id string) error {
	if _, err := p.ownUser(ctx, id); err != nil {
		return err
	}
	return p.d.DisableUser(ctx, id)
}

// EnableUser re-enables one of the source's accounts.
func (p *Provisioner) EnableUser(ctx context.Context, id string) error {
	if _, err := p.ownUser(ctx, id); err != nil {
		return err
	}
	return p.d.EnableUser(ctx, id)
}

// DeleteUser deletes one of the source's accounts.
func (p *Provisioner) DeleteUser(ctx context.Context, id string) error {
	if _, err := p.ownUser(ctx, id); err != nil {
		return err
	}
	return p.d.DeleteUser(ctx, id)
}
