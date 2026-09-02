// store.go is the data-access layer for PolicyBinding. Spec.ProjectID,
// Spec.PolicyID and Spec.Priority live in their own columns (FK cascade,
// project index) and Spec.Subjects in the policy_binding_subjects junction,
// where a subject with an id also fills the matching FK column so a deleted
// principal drops out. Upsert fans out across both tables in one
// transaction.
package policybinding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/internal/storage/gen"
)

// Store is the PolicyBinding data-access type. Holds a pool so Upsert can
// run a multi-table transaction.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore constructs a Store bound to a pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// List returns every PolicyBinding row, hydrating Subjects from the junction.
func (s *Store) List(ctx context.Context) ([]*PolicyBinding, error) {
	q := gen.New(s.pool)
	rows, err := q.ListPolicyBindings(ctx)
	if err != nil {
		return nil, fmt.Errorf("policybinding.List: %w", err)
	}
	subjectRows, err := q.ListPolicyBindingSubjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("policybinding.List subjects: %w", err)
	}
	byBinding := map[string][]rolebinding.Subject{}
	for _, r := range subjectRows {
		byBinding[r.BindingID] = append(byBinding[r.BindingID], subjectFromRow(r.Kind, r.SubjectID, r.SubjectName))
	}
	out := make([]*PolicyBinding, 0, len(rows))
	for _, r := range rows {
		b, err := fromRow(r)
		if err != nil {
			return nil, fmt.Errorf("policybinding %s: %w", r.Name, err)
		}
		b.Spec.Subjects = byBinding[b.Meta.ID]
		out = append(out, b)
	}
	return out, nil
}

// Get returns the PolicyBinding with the given id, or (nil, nil) if not found.
func (s *Store) Get(ctx context.Context, id string) (*PolicyBinding, error) {
	q := gen.New(s.pool)
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
	subjectRows, err := q.GetPolicyBindingSubjects(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("policybinding.Get subjects: %w", err)
	}
	for _, sr := range subjectRows {
		b.Spec.Subjects = append(b.Spec.Subjects, subjectFromRow(sr.Kind, sr.SubjectID, sr.SubjectName))
	}
	return b, nil
}

// Upsert writes b across policy_bindings + policy_binding_subjects in one
// tx. Owner is re-derived from ProjectID.
func (s *Store) Upsert(ctx context.Context, b *PolicyBinding) error {
	b.StampOwner()
	params, err := toUpsertParams(b)
	if err != nil {
		return fmt.Errorf("policybinding.Upsert: %w", err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("policybinding.Upsert: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := gen.New(tx)

	if err := q.UpsertPolicyBinding(ctx, params); err != nil {
		return fmt.Errorf("policybinding.Upsert: policy_bindings: %w", err)
	}
	if err := q.DeletePolicyBindingSubjects(ctx, b.Meta.ID); err != nil {
		return fmt.Errorf("policybinding.Upsert: clear subjects: %w", err)
	}
	for i := range b.Spec.Subjects {
		sub := &b.Spec.Subjects[i]
		if err := q.InsertPolicyBindingSubject(ctx, gen.InsertPolicyBindingSubjectParams{
			BindingID:     b.Meta.ID,
			Kind:          string(sub.Kind),
			SubjectID:     text(sub.ID),
			SubjectName:   text(sub.Name),
			SubjectUserID: text(subjectFK(sub, rolebinding.SubjectUser)),
			SubjectSaID:   text(subjectFK(sub, rolebinding.SubjectServiceAccount)),
			Position:      int32(i),
		}); err != nil {
			return fmt.Errorf("policybinding.Upsert: insert subject %s: %w", sub.Key(), err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("policybinding.Upsert: commit: %w", err)
	}
	return nil
}

// Delete removes a PolicyBinding by id. Junction rows cascade via FK.
func (s *Store) Delete(ctx context.Context, id string) error {
	return gen.New(s.pool).DeletePolicyBinding(ctx, id)
}

// subjectFK returns the subject id when the subject is of kind want, so the
// matching FK column carries it and the row cascades with the principal.
func subjectFK(s *rolebinding.Subject, want rolebinding.SubjectKind) string {
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

func subjectFromRow(kind string, id, name pgtype.Text) rolebinding.Subject {
	return rolebinding.Subject{Kind: rolebinding.SubjectKind(kind), ID: id.String, Name: name.String}
}

func fromRow(r gen.PolicyBinding) (*PolicyBinding, error) {
	md, err := meta.UnmarshalJSONB(r.ID, r.Name, r.DisplayName, r.Metadata)
	if err != nil {
		return nil, err
	}
	md.CreatedAt = r.CreatedAt.Time
	md.UpdatedAt = r.UpdatedAt.Time
	var sp Spec
	if err := json.Unmarshal(r.Spec, &sp); err != nil {
		return nil, fmt.Errorf("spec: %w", err)
	}
	sp.ProjectID = r.ProjectID
	sp.PolicyID = r.PolicyID
	prio := int(r.Priority)
	sp.Priority = &prio
	sp.Subjects = nil
	b := &PolicyBinding{Meta: md, Spec: sp}
	b.StampOwner()
	return b, nil
}

func toUpsertParams(b *PolicyBinding) (gen.UpsertPolicyBindingParams, error) {
	metaJSON, err := meta.MarshalJSONB(b.Meta)
	if err != nil {
		return gen.UpsertPolicyBindingParams{}, fmt.Errorf("metadata: %w", err)
	}
	// Relational fields live in columns / the junction; keep them out of the
	// spec JSONB so there is one source of truth per field.
	stored := b.Spec
	stored.ProjectID = ""
	stored.PolicyID = ""
	stored.Priority = nil
	stored.Subjects = nil
	specJSON, err := json.Marshal(stored)
	if err != nil {
		return gen.UpsertPolicyBindingParams{}, fmt.Errorf("spec: %w", err)
	}
	return gen.UpsertPolicyBindingParams{
		ID:          b.Meta.ID,
		Name:        b.Meta.Name,
		DisplayName: b.Meta.DisplayName,
		ProjectID:   b.Spec.ProjectID,
		PolicyID:    b.Spec.PolicyID,
		Priority:    int32(b.EffectivePriority()),
		Metadata:    metaJSON,
		Spec:        specJSON,
	}, nil
}
