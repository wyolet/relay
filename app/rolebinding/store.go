// Subjects live in the role_binding_subjects junction, where a subject with an
// id also fills the matching FK column so deleting a user or service account
// drops it from every binding.

package rolebinding

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

func (s *Store) List(ctx context.Context) ([]*RoleBinding, error) {
	q := gen.New(s.db)
	rows, err := q.ListRoleBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("rolebinding.List: %w", err)
	}
	subjectRows, err := q.ListRoleBindingSubjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("rolebinding.List subjects: %w", err)
	}
	byBinding := map[string][]Subject{}
	for _, r := range subjectRows {
		byBinding[r.BindingID] = append(byBinding[r.BindingID], subjectFromRow(r.Kind, r.SubjectID, r.SubjectName))
	}
	out := make([]*RoleBinding, 0, len(rows))
	for _, r := range rows {
		b, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("rolebinding %s: %w", r.Name, err)
		}
		b.Spec.Subjects = byBinding[b.Meta.ID]
		out = append(out, b)
	}
	return out, nil
}

// Get returns (nil, nil) when no row has id.
func (s *Store) Get(ctx context.Context, id string) (*RoleBinding, error) {
	q := gen.New(s.db)
	r, err := q.GetRoleBinding(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("rolebinding.Get: %w", err)
	}
	b, err := fromRow(r)
	if err != nil {
		return nil, err
	}
	subjectRows, err := q.GetRoleBindingSubjects(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("rolebinding.Get subjects: %w", err)
	}
	for _, sr := range subjectRows {
		b.Spec.Subjects = append(b.Spec.Subjects, subjectFromRow(sr.Kind, sr.SubjectID, sr.SubjectName))
	}
	return b, nil
}

// Upsert re-derives Owner from Spec.Scope.
func (s *Store) Upsert(ctx context.Context, b *RoleBinding) error {
	b.StampOwner()
	params, err := toUpsertParams(b)
	if err != nil {
		return fmt.Errorf("rolebinding.Upsert: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("rolebinding.Upsert: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	if err := meta.StaleIfNoRows(q.UpsertRoleBinding(ctx, params)); err != nil {
		return fmt.Errorf("rolebinding.Upsert: role_bindings: %w", err)
	}
	if err := q.DeleteRoleBindingSubjects(ctx, b.Meta.ID); err != nil {
		return fmt.Errorf("rolebinding.Upsert: clear subjects: %w", err)
	}
	for i := range b.Spec.Subjects {
		sub := &b.Spec.Subjects[i]
		if err := q.InsertRoleBindingSubject(ctx, gen.InsertRoleBindingSubjectParams{
			BindingID:     b.Meta.ID,
			Kind:          string(sub.Kind),
			SubjectID:     text(sub.ID),
			SubjectName:   text(sub.Name),
			SubjectUserID: text(subjectFK(sub, SubjectUser)),
			SubjectSaID:   text(subjectFK(sub, SubjectServiceAccount)),
			Position:      int32(i),
		}); err != nil {
			return fmt.Errorf("rolebinding.Upsert: insert subject %s: %w", sub.Key(), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("rolebinding.Upsert: commit: %w", err)
	}
	return nil
}

// Delete relies on the FK to cascade the junction rows.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.db).DeleteRoleBinding(ctx, id)
}

// subjectFK returns the subject id when the subject is of kind want, so the
// matching FK column carries it and the row cascades with the principal.
func subjectFK(s *Subject, want SubjectKind) string {
	if s.Kind != want {
		return ""
	}
	return s.ID
}

func text(s string) pgtype.Text {
	if s == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: s, Valid: true}
}

func subjectFromRow(kind string, id, name pgtype.Text) Subject {
	return Subject{Kind: SubjectKind(kind), ID: id.String, Name: name.String}
}

func fromRow(r gen.RoleBinding) (*RoleBinding, error) {
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
	sp.RoleID = r.RoleID
	sp.Scope = meta.Owner{Kind: meta.OwnerKind(r.ScopeKind), ID: r.ScopeID.String}
	sp.Subjects = nil
	b := &RoleBinding{Meta: md, Spec: sp}
	b.StampOwner()
	return b, nil
}

func toUpsertParams(b *RoleBinding) (gen.UpsertRoleBindingParams, error) {
	metaJSON, err := meta.MarshalJSONB(b.Meta)
	if err != nil {
		return gen.UpsertRoleBindingParams{}, fmt.Errorf("metadata: %w", err)
	}
	// Relational fields live in columns / the junction; keep them out of the
	// spec JSONB so there is one source of truth per field.
	stored := b.Spec
	stored.RoleID = ""
	stored.Scope = meta.Owner{}
	stored.Subjects = nil
	specJSON, err := json.Marshal(stored)
	if err != nil {
		return gen.UpsertRoleBindingParams{}, fmt.Errorf("spec: %w", err)
	}
	ver, ok, err := b.Meta.ExpectedVersion()
	if err != nil {
		return gen.UpsertRoleBindingParams{}, err
	}
	return gen.UpsertRoleBindingParams{
		ID:              b.Meta.ID,
		Name:            b.Meta.Name,
		DisplayName:     b.Meta.DisplayName,
		RoleID:          b.Spec.RoleID,
		ScopeKind:       string(b.Spec.Scope.Kind),
		ScopeID:         text(b.Spec.Scope.ID),
		Metadata:        metaJSON,
		Spec:            specJSON,
		ExpectedVersion: pgtype.Int8{Int64: ver, Valid: ok},
	}, nil
}
