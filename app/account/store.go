// Spec.HostID lives in the host_id column so a host delete cascades; Spec.Default lives in default_owner, which holds the owner of a default account so one owner has at most one per host. The spec JSONB carries the rest.

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

const oneDefaultIndex = "accounts_one_default"

// ensureDefaultAttempts bounds the retries when a concurrent save takes the minted name.
const ensureDefaultAttempts = 3

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
	if storage.IsUniqueViolation(err, oneDefaultIndex) {
		return fmt.Errorf("%w: account %q", ErrDefaultTaken, a.Meta.Name)
	}
	return meta.StaleIfNoRows(n, err)
}

// Delete removes an Account by id.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.q.DeleteAccount(ctx, id)
}

// EnsureDefault returns the id of owner's default account at hostID, creating it on first use: named after the host, suffixed -2, -3, … when the name is taken. Concurrent first saves converge on one row. It returns "" when the host does not exist, since an account cannot outlive its host.
func (s *Store) EnsureDefault(ctx context.Context, owner meta.Owner, hostID string) (string, error) {
	if namesNobody(owner) {
		owner = meta.Owner{Kind: meta.OwnerSystem}
	}
	lookup := gen.GetDefaultAccountIDParams{HostID: hostID, DefaultOwner: pgtype.Text{String: defaultOwner(owner), Valid: true}}
	for attempt := 0; ; attempt++ {
		id, err := s.q.GetDefaultAccountID(ctx, lookup)
		if err == nil {
			return id, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("account.EnsureDefault: %w", err)
		}
		if attempt == ensureDefaultAttempts {
			return "", fmt.Errorf("account.EnsureDefault: no default account for %s at host %s after %d attempts", lookup.DefaultOwner.String, hostID, attempt)
		}
		h, err := s.q.GetHost(ctx, hostID)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("account.EnsureDefault: host: %w", err)
		}
		var nameErr error
		name := slug.Unique(h.Name, func(c string) bool {
			taken, err := s.q.AccountNameTaken(ctx, c)
			if err != nil {
				nameErr = err
				return false
			}
			return taken
		})
		if nameErr != nil {
			return "", fmt.Errorf("account.EnsureDefault: name: %w", nameErr)
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
	spec.HostID = r.HostID
	spec.Default = r.DefaultOwner.Valid
	return &Account{Meta: md, Spec: spec}, nil
}

func toUpsertParams(a *Account) (gen.UpsertAccountParams, error) {
	metaJSON, err := meta.MarshalJSONB(a.Meta)
	if err != nil {
		return gen.UpsertAccountParams{}, fmt.Errorf("metadata: %w", err)
	}
	stored := a.Spec
	stored.HostID = ""
	stored.Default = false
	specJSON, err := json.Marshal(stored)
	if err != nil {
		return gen.UpsertAccountParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := a.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertAccountParams{}, err
	}
	var def pgtype.Text
	if a.Spec.Default {
		def = pgtype.Text{String: defaultOwner(a.Meta.Owner), Valid: true}
	}
	return gen.UpsertAccountParams{
		ID:              a.Meta.ID,
		Name:            a.Meta.Name,
		DisplayName:     a.Meta.DisplayName,
		HostID:          a.Spec.HostID,
		Metadata:        metaJSON,
		Spec:            specJSON,
		DefaultOwner:    def,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
