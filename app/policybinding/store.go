// Accounts live in the policy_binding_accounts junction, whose FK drops a deleted account from its binding and whose unique index holds an account to one binding. The owner lives in the project_id, team_id and user_id columns, so a deleted owner takes its bindings along.

package policybinding

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
)

// ErrAccountBound is a write naming an account another binding already holds: an account is bound to at most one policy.
var ErrAccountBound = errors.New("policybinding: account is already bound by another policy binding")

// oneBindingIndex holds an account to one binding.
const oneBindingIndex = "policy_binding_accounts_one_binding"

// Store holds a DB rather than Queries so Upsert can run a multi-table
// transaction.
type Store struct {
	db storage.DB
}

func NewStore(db storage.DB) *Store { return &Store{db: db} }

func (s *Store) List(ctx context.Context) ([]*PolicyBinding, error) {
	q := gen.New(s.db)
	rows, err := q.ListPolicyBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("policybinding.List: %w", err)
	}
	accountRows, err := q.ListPolicyBindingAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("policybinding.List accounts: %w", err)
	}
	byBinding := map[string][]string{}
	for _, r := range accountRows {
		byBinding[r.BindingID] = append(byBinding[r.BindingID], r.AccountID)
	}
	out := make([]*PolicyBinding, 0, len(rows))
	for _, r := range rows {
		b, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("policybinding %s: %w", r.Name, err)
		}
		b.Spec.AccountIDs = byBinding[b.Meta.ID]
		out = append(out, b)
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*PolicyBinding, error) {
	q := gen.New(s.db)
	r, err := q.GetPolicyBinding(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("policybinding.Get: %w", err)
	}
	b, err := fromRow(r)
	if err != nil {
		return nil, err
	}
	accountRows, err := q.GetPolicyBindingAccounts(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("policybinding.Get accounts: %w", err)
	}
	for _, ar := range accountRows {
		b.Spec.AccountIDs = append(b.Spec.AccountIDs, ar.AccountID)
	}
	return b, nil
}

// Holding returns the binding that holds accountID, nil when none does.
func (s *Store) Holding(ctx context.Context, accountID string) (*PolicyBinding, error) {
	id, err := gen.New(s.db).GetPolicyBindingIDForAccount(ctx, accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("policybinding.Holding: %w", err)
	}
	return s.Get(ctx, id)
}

// Upsert writes the binding and its accounts in one transaction.
func (s *Store) Upsert(ctx context.Context, b *PolicyBinding) error {
	switch b.Meta.Owner.Kind {
	case meta.OwnerSystem, meta.OwnerTeam, meta.OwnerProject, meta.OwnerUser:
	default:
		return fmt.Errorf("policybinding.Upsert: owner.kind must be system, team, project or user, got %q", b.Meta.Owner.Kind)
	}
	params, err := toUpsertParams(b)
	if err != nil {
		return fmt.Errorf("policybinding.Upsert: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("policybinding.Upsert: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	if err := meta.StaleIfNoRows(q.UpsertPolicyBinding(ctx, params)); err != nil {
		return fmt.Errorf("policybinding.Upsert: policy_bindings: %w", err)
	}
	if err := q.DeletePolicyBindingAccounts(ctx, b.Meta.ID); err != nil {
		return fmt.Errorf("policybinding.Upsert: clear accounts: %w", err)
	}
	for i, id := range b.Spec.AccountIDs {
		if err := q.InsertPolicyBindingAccount(ctx, gen.InsertPolicyBindingAccountParams{BindingID: b.Meta.ID, AccountID: id, Position: int32(i)}); err != nil {
			if storage.IsUniqueViolation(err, oneBindingIndex) {
				return fmt.Errorf("%w: account %s", ErrAccountBound, id)
			}
			return fmt.Errorf("policybinding.Upsert: insert account %s: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("policybinding.Upsert: commit: %w", err)
	}
	return nil
}

// Delete relies on the FK to cascade the junction rows.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.db).DeletePolicyBinding(ctx, id)
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func fromRow(r gen.PolicyBinding) (*PolicyBinding, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	md.ResourceVersion = meta.FormatResourceVersion(r.ResourceVersion)
	var sp Spec
	if err := json.Unmarshal(r.Spec, &sp); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	sp.PolicyID = r.PolicyID
	sp.AccountIDs = nil
	// The owner columns are the truth; the metadata copy is ignored.
	switch {
	case r.ProjectID.Valid:
		md.Owner = meta.Owner{Kind: meta.OwnerProject, ID: r.ProjectID.String}
	case r.TeamID.Valid:
		md.Owner = meta.Owner{Kind: meta.OwnerTeam, ID: r.TeamID.String}
	case r.UserID.Valid:
		md.Owner = meta.Owner{Kind: meta.OwnerUser, ID: r.UserID.String}
	default:
		md.Owner = meta.Owner{Kind: meta.OwnerSystem}
	}
	return &PolicyBinding{Meta: md, Spec: sp}, nil
}

func toUpsertParams(b *PolicyBinding) (gen.UpsertPolicyBindingParams, error) {
	metaJSON, err := meta.MarshalJSONB(b.Meta)
	if err != nil {
		return gen.UpsertPolicyBindingParams{}, fmt.Errorf("metadata: %w", err)
	}
	// Relational fields live in columns and the junction; the document keeps the rest.
	specJSON, err := json.Marshal(Spec{Enabled: b.Spec.Enabled})
	if err != nil {
		return gen.UpsertPolicyBindingParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := b.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertPolicyBindingParams{}, err
	}
	var projectID, teamID, userID string
	switch b.Meta.Owner.Kind {
	case meta.OwnerProject:
		projectID = b.Meta.Owner.ID
	case meta.OwnerTeam:
		teamID = b.Meta.Owner.ID
	case meta.OwnerUser:
		userID = b.Meta.Owner.ID
	}
	return gen.UpsertPolicyBindingParams{
		ID:              b.Meta.ID,
		Name:            b.Meta.Name,
		DisplayName:     b.Meta.DisplayName,
		ProjectID:       text(projectID),
		TeamID:          text(teamID),
		UserID:          text(userID),
		PolicyID:        b.Spec.PolicyID,
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
