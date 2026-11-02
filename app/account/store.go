// Spec.HostID lives in the host_id column so a host delete cascades; Spec.Default lives in default_owner, which holds the owner of a default account so one owner has at most one per host. A caller account's kind, holder, scope and managed key are columns too, so a deleted team, project, user or service account takes its accounts along and one place holds one row. The spec JSONB carries the rest.

package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/slug"
)

// ErrDefaultTaken is a write marking a second default account for one owner at one host.
var ErrDefaultTaken = errors.New("account: the owner already has a default account at this host")

// ErrManagedTaken is a write of a caller account whose place another row holds.
var ErrManagedTaken = errors.New("account: another account already holds this place")

const (
	oneDefaultIndex = "accounts_one_default"
	oneManagedIndex = "accounts_managed_key_key"
)

// ensureAttempts bounds the retries when a concurrent save takes the minted name.
const ensureAttempts = 3

// Store is the Account data-access type.
type Store struct {
	q *gen.Queries
}

// NewStore constructs a Store from an existing sqlc Queries handle.
func NewStore(q *gen.Queries) *Store { return &Store{q: q} }

// List returns every Account row.
func (s *Store) List(ctx context.Context) ([]*Account, error) {
	rows, err := s.q.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("account.List: %w", err)
	}
	out := make([]*Account, 0, len(rows))
	for _, r := range rows {
		a, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", r.Name, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// Get returns the Account with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*Account, error) {
	r, err := s.q.GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("account.Get: %w", err)
	}
	return fromRow(r)
}

// Upsert writes a. The caller sets Meta.ID.
func (s *Store) Upsert(ctx context.Context, a *Account) error {
	params, err := toUpsertParams(a)
	if err != nil {
		return fmt.Errorf("account.Upsert: %w", err)
	}
	n, err := s.q.UpsertAccount(ctx, params)
	switch {
	case storage.IsUniqueViolation(err, oneDefaultIndex):
		return fmt.Errorf("%w: account %q", ErrDefaultTaken, a.Meta.Name)
	case storage.IsUniqueViolation(err, oneManagedIndex):
		return fmt.Errorf("%w: account %q", ErrManagedTaken, a.Meta.Name)
	}
	return meta.StaleIfNoRows(n, err)
}

// Delete removes an Account by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteAccount(ctx, id)
}

// EnsureManaged returns the id of the caller account want describes, creating it on first use with want's name, suffixed -2, -3, … when taken. Concurrent first saves converge on one row.
func (s *Store) EnsureManaged(ctx context.Context, want *Account) (string, error) {
	key := want.ManagedKey()
	if key == "" {
		return "", fmt.Errorf("account.EnsureManaged: %q is not a caller account", want.Meta.Name)
	}
	lookup := pgtype.Text{String: key, Valid: true}
	for attempt := 0; ; attempt++ {
		id, err := s.q.GetManagedAccountID(ctx, lookup)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("account.EnsureManaged: %w", err)
		}
		if attempt == ensureAttempts {
			return "", fmt.Errorf("account.EnsureManaged: no account for %s after %d attempts", key, attempt)
		}
		name, err := s.uniqueName(ctx, want.Meta.Name)
		if err != nil {
			return "", fmt.Errorf("account.EnsureManaged: %w", err)
		}
		a := *want
		a.Meta.ID, a.Meta.Name = meta.NewID(), name
		p, err := toUpsertParams(&a)
		if err != nil {
			return "", fmt.Errorf("account.EnsureManaged: %w", err)
		}
		if _, err := s.q.InsertManagedAccount(ctx, gen.InsertManagedAccountParams{
			ID: p.ID, Name: p.Name, DisplayName: p.DisplayName, Metadata: p.Metadata, Spec: p.Spec,
			Kind: p.Kind, ManagedKey: p.ManagedKey, HolderUserID: p.HolderUserID, HolderSaID: p.HolderSaID,
			TeamID: p.TeamID, ProjectID: p.ProjectID, FollowsRoleBindings: p.FollowsRoleBindings,
		}); err != nil {
			return "", fmt.Errorf("account.EnsureManaged: insert: %w", err)
		}
	}
}

// SetMembership marks a member account closed or open and whether it follows role bindings; a row already so is left alone.
func (s *Store) SetMembership(ctx context.Context, id string, closed, followsRoleBindings bool) error {
	_, err := s.q.SetAccountMembership(ctx, gen.SetAccountMembershipParams{ID: id, Closed: closed, FollowsRoleBindings: followsRoleBindings})
	if err != nil {
		return fmt.Errorf("account.SetMembership: %w", err)
	}
	return nil
}

// EnsureDefault returns the id of owner's default account at hostID, creating it on first use: named after the host, suffixed -2, -3, … when the name is taken. Concurrent first saves converge on one row. It returns "" when the host does not exist, since an account cannot outlive its host.
func (s *Store) EnsureDefault(ctx context.Context, owner meta.Owner, hostID string) (string, error) {
	if namesNobody(owner) {
		owner = meta.Owner{Kind: meta.OwnerSystem}
	}
	lookup := gen.GetDefaultAccountIDParams{HostID: text(hostID), DefaultOwner: text(defaultOwner(owner))}
	for attempt := 0; ; attempt++ {
		id, err := s.q.GetDefaultAccountID(ctx, lookup)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("account.EnsureDefault: %w", err)
		}
		if attempt == ensureAttempts {
			return "", fmt.Errorf("account.EnsureDefault: no default account for %s at host %s after %d attempts", lookup.DefaultOwner.String, hostID, attempt)
		}
		h, err := s.q.GetHost(ctx, hostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("account.EnsureDefault: host: %w", err)
		}
		name, err := s.uniqueName(ctx, h.Name)
		if err != nil {
			return "", fmt.Errorf("account.EnsureDefault: %w", err)
		}
		a := &Account{
			Meta: meta.Metadata{ID: meta.NewID(), Name: name, DisplayName: h.DisplayName, Owner: owner},
			Spec: Spec{HostID: hostID, Default: true},
		}
		p, err := toUpsertParams(a)
		if err != nil {
			return "", fmt.Errorf("account.EnsureDefault: %w", err)
		}
		if _, err := s.q.InsertDefaultAccount(ctx, gen.InsertDefaultAccountParams{
			ID: p.ID, Name: p.Name, DisplayName: p.DisplayName, HostID: p.HostID,
			Metadata: p.Metadata, Spec: p.Spec, DefaultOwner: p.DefaultOwner,
		}); err != nil {
			return "", fmt.Errorf("account.EnsureDefault: insert: %w", err)
		}
	}
}

func (s *Store) uniqueName(ctx context.Context, base string) (string, error) {
	var nameErr error
	name := slug.Unique(base, func(c string) bool {
		taken, err := s.q.AccountNameTaken(ctx, c)
		if err != nil {
			nameErr = err
			return false
		}
		return taken
	})
	if nameErr != nil {
		return "", fmt.Errorf("name: %w", nameErr)
	}
	return name, nil
}

// defaultOwner is the default_owner value of o's default account; migration 000036 writes the same form.
func defaultOwner(o meta.Owner) string {
	if namesNobody(o) || o.Kind == meta.OwnerSystem {
		return string(meta.OwnerSystem)
	}
	return string(o.Kind) + ":" + o.ID
}

// namesNobody reports an owner the write paths stamp as the system: no kind, or a user without an id (refcheck.StampOwnerID).
func namesNobody(o meta.Owner) bool {
	return o.Kind == "" || (o.Kind == meta.OwnerUser && o.ID == "")
}

func fromRow(r gen.Account) (*Account, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var spec Spec
	if err := json.Unmarshal(r.Spec, &spec); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	spec.HostID = r.HostID.String
	spec.Default = r.DefaultOwner.Valid
	spec.Kind = Kind(r.Kind)
	spec.Closed = r.Closed
	spec.Holder = nil
	switch {
	case r.HolderUserID.Valid:
		spec.Holder = &Holder{Kind: HolderUser, ID: r.HolderUserID.String}
	case r.HolderSaID.Valid:
		spec.Holder = &Holder{Kind: HolderServiceAccount, ID: r.HolderSaID.String}
	}
	return &Account{Meta: md, Spec: spec, FollowsRoleBindings: r.FollowsRoleBindings}, nil
}

func toUpsertParams(a *Account) (gen.UpsertAccountParams, error) {
	metaJSON, err := meta.MarshalJSONB(a.Meta)
	if err != nil {
		return gen.UpsertAccountParams{}, fmt.Errorf("metadata: %w", err)
	}
	// Only the flags with no column of their own stay in the document.
	stored := Spec{PayloadLogging: a.Spec.PayloadLogging, OrgID: a.Spec.OrgID}
	specJSON, err := json.Marshal(stored)
	if err != nil {
		return gen.UpsertAccountParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := a.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertAccountParams{}, err
	}
	kind := a.DerivedKind()
	if kind == "" {
		return gen.UpsertAccountParams{}, fmt.Errorf("account %q fits no kind", a.Meta.Name)
	}
	p := gen.UpsertAccountParams{
		ID:                  a.Meta.ID,
		Name:                a.Meta.Name,
		DisplayName:         a.Meta.DisplayName,
		HostID:              text(a.Spec.HostID),
		Metadata:            metaJSON,
		Spec:                specJSON,
		Kind:                string(kind),
		ManagedKey:          text(a.ManagedKey()),
		Closed:              a.Spec.Closed,
		FollowsRoleBindings: a.FollowsRoleBindings,
		ExpectedVersion:     pgtype.Int8{Int64: ver, Valid: ok},
	}
	if a.Spec.Default && kind == KindProvider {
		p.DefaultOwner = text(defaultOwner(a.Meta.Owner))
	}
	if h := a.Spec.Holder; h != nil {
		switch h.Kind {
		case HolderUser:
			p.HolderUserID = text(h.ID)
		case HolderServiceAccount:
			p.HolderSaID = text(h.ID)
		}
	}
	if kind != KindProvider {
		switch a.Meta.Owner.Kind {
		case meta.OwnerTeam:
			p.TeamID = text(a.Meta.Owner.ID)
		case meta.OwnerProject:
			p.ProjectID = text(a.Meta.Owner.ID)
		}
	}
	return p, nil
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}
