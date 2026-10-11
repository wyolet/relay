// Subjects live in role_binding_subjects. A subject with an id also fills the foreign-key column of its kind, so deleting a user or service account drops it from every binding.

package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

type bindings struct{ db DB }

func (s bindings) Get(ctx context.Context, id string) (*directory.RoleBinding, error) {
	q := gen.New(s.db)
	r, err := q.GetRoleBinding(ctx, id)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("get role binding", err)
	}
	b, err := bindingFromRow(r)
	if err != nil {
		return nil, err
	}
	subjects, err := q.GetRoleBindingSubjects(ctx, id)
	if err != nil {
		return nil, mapErr("get role binding subjects", err)
	}
	for _, sr := range subjects {
		b.Spec.Subjects = append(b.Spec.Subjects, subjectFromRow(sr.Kind, sr.SubjectID, sr.SubjectName))
	}
	return b, nil
}

func (s bindings) List(ctx context.Context) ([]*directory.RoleBinding, error) {
	q := gen.New(s.db)
	rows, err := q.ListRoleBindings(ctx)
	if err != nil {
		return nil, mapErr("list role bindings", err)
	}
	subjects, err := q.ListRoleBindingSubjects(ctx)
	if err != nil {
		return nil, mapErr("list role binding subjects", err)
	}
	byBinding := map[string][]directory.Subject{}
	for _, sr := range subjects {
		byBinding[sr.BindingID] = append(byBinding[sr.BindingID], subjectFromRow(sr.Kind, sr.SubjectID, sr.SubjectName))
	}
	out, err := convert(rows, bindingFromRow)
	if err != nil {
		return nil, err
	}
	for _, b := range out {
		b.Spec.Subjects = byBinding[b.Meta.ID]
	}
	return out, nil
}

func (s bindings) Upsert(ctx context.Context, b *directory.RoleBinding) error {
	b.StampOwner()
	md, err := encodeMetadata(b.Meta)
	if err != nil {
		return err
	}
	spec, err := encodeSpec(bindingSpecDoc{Enabled: b.Spec.Enabled}, b.Spec.Attributes, bindingSpecKeys)
	if err != nil {
		return err
	}
	ver, err := expectedVersion(b.Meta)
	if err != nil {
		return err
	}
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		if err := mapErr("upsert role binding", staleIfNoRows(q.UpsertRoleBinding(ctx, gen.UpsertRoleBindingParams{
			ID: b.Meta.ID, Name: b.Meta.Name, DisplayName: b.Meta.DisplayName, RoleID: b.Spec.RoleID,
			ScopeKind: b.Spec.Scope.Kind, ScopeID: text(b.Spec.Scope.ID),
			Metadata: md, Spec: spec, ExpectedVersion: ver,
		}))); err != nil {
			return err
		}
		if err := q.DeleteRoleBindingSubjects(ctx, b.Meta.ID); err != nil {
			return mapErr("clear role binding subjects", err)
		}
		for i, sub := range b.Spec.Subjects {
			if err := q.InsertRoleBindingSubject(ctx, gen.InsertRoleBindingSubjectParams{
				BindingID:     b.Meta.ID,
				Kind:          string(sub.Kind),
				SubjectID:     text(sub.ID),
				SubjectName:   text(sub.Name),
				SubjectUserID: text(subjectFK(sub, directory.SubjectUser)),
				SubjectSaID:   text(subjectFK(sub, directory.SubjectServiceAccount)),
				Position:      int32(i),
			}); err != nil {
				return mapErr("insert role binding subject "+sub.Key(), err)
			}
		}
		return nil
	})
}

func (s bindings) Delete(ctx context.Context, id string) error {
	return mapErr("delete role binding", gen.New(s.db).DeleteRoleBinding(ctx, id))
}

// subjectFK returns the subject's id when it is of kind want, for the foreign-key column of that kind.
func subjectFK(s directory.Subject, want directory.SubjectKind) string {
	if s.Kind != want {
		return ""
	}
	return s.ID
}

func subjectFromRow(kind string, id, name pgtype.Text) directory.Subject {
	return directory.Subject{Kind: directory.SubjectKind(kind), ID: id.String, Name: name.String}
}

func bindingFromRow(r gen.RoleBinding) (*directory.RoleBinding, error) {
	m, err := decodeMetadata(r.ID, r.Name, r.DisplayName, r.Metadata, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
	if err != nil {
		return nil, err
	}
	var doc bindingSpecDoc
	attrs, err := decodeSpec(r.Spec, &doc, bindingSpecKeys)
	if err != nil {
		return nil, err
	}
	b := &directory.RoleBinding{Meta: m, Spec: directory.RoleBindingSpec{
		RoleID:     r.RoleID,
		Scope:      rbac.Scope{Kind: r.ScopeKind, ID: r.ScopeID.String},
		Enabled:    doc.Enabled,
		Attributes: attrs,
	}}
	b.StampOwner()
	return b, nil
}
