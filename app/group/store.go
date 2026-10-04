// Spec.MemberIDs lives in the group_members junction, not the spec JSONB, so
// the FK to users drops a deleted user's memberships.

package group

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

// Store holds a DB rather than Queries so Upsert can run a multi-table
// transaction.
type Store struct {
	db storage.DB
}

func NewStore(db storage.DB) *Store { return &Store{db: db} }

func (s *Store) List(ctx context.Context) ([]*Group, error) {
	q := gen.New(s.db)
	rows, err := q.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("group.List: %w", err)
	}
	memberRows, err := q.ListGroupMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("group.List members: %w", err)
	}
	membersByGroup := map[string][]string{}
	for _, r := range memberRows {
		membersByGroup[r.GroupID] = append(membersByGroup[r.GroupID], r.UserID)
	}
	out := make([]*Group, 0, len(rows))
	for _, r := range rows {
		g, err := fromRow(r.ID, r.Name, r.DisplayName, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("group %s: %w", r.Name, err)
		}
		g.Spec.MemberIDs = membersByGroup[g.Meta.ID]
		out = append(out, g)
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*Group, error) {
	q := gen.New(s.db)
	r, err := q.GetGroup(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("group.Get: %w", err)
	}
	g, err := fromRow(r.ID, r.Name, r.DisplayName, r.Metadata, r.Spec, r.CreatedAt, r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	memberRows, err := q.GetGroupMembers(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("group.Get members: %w", err)
	}
	for _, mr := range memberRows {
		g.Spec.MemberIDs = append(g.Spec.MemberIDs, mr.UserID)
	}
	return g, nil
}

func (s *Store) Upsert(ctx context.Context, g *Group) error {
	params, err := toUpsertParams(g)
	if err != nil {
		return fmt.Errorf("group.Upsert: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("group.Upsert: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	if err := q.UpsertGroup(ctx, params); err != nil {
		return fmt.Errorf("group.Upsert: groups: %w", err)
	}
	if err := q.DeleteGroupMembers(ctx, g.Meta.ID); err != nil {
		return fmt.Errorf("group.Upsert: clear members: %w", err)
	}
	for i, id := range g.Spec.MemberIDs {
		if err := q.InsertGroupMember(ctx, gen.InsertGroupMemberParams{
			GroupID:  g.Meta.ID,
			UserID:   id,
			Position: int32(i),
		}); err != nil {
			return fmt.Errorf("group.Upsert: insert member %s: %w", id, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("group.Upsert: commit: %w", err)
	}
	return nil
}

// Delete relies on the FK to cascade the junction rows.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.db).DeleteGroup(ctx, id)
}

func fromRow(id, name, displayName string, metadata, spec []byte, createdAt, updatedAt pgtype.Timestamptz) (*Group, error) {
	md, err := meta.UnmarshalJSONB(id, name, displayName, metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = createdAt.Time
	md.UpdatedAt = updatedAt.Time
	var sp Spec
	if err := json.Unmarshal(spec, &sp); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	sp.MemberIDs = nil
	return &Group{Meta: md, Spec: sp}, nil
}

func toUpsertParams(g *Group) (gen.UpsertGroupParams, error) {
	metaJSON, err := meta.MarshalJSONB(g.Meta)
	if err != nil {
		return gen.UpsertGroupParams{}, fmt.Errorf("metadata: %w", err)
	}
	// MemberIDs are relational; keep them out of the spec JSONB so the
	// junction stays the single source of truth.
	stored := *g
	stored.Spec.MemberIDs = nil
	specJSON, err := json.Marshal(stored.Spec)
	if err != nil {
		return gen.UpsertGroupParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertGroupParams{
		ID:          g.Meta.ID,
		Name:        g.Meta.Name,
		DisplayName: g.Meta.DisplayName,
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
