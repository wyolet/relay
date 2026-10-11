package postgres

import (
	"context"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/rbac"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

type roles struct{ db DB }

func (s roles) Get(ctx context.Context, id string) (*directory.Role, error) {
	r, err := gen.New(s.db).GetRole(ctx, id)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("get role", err)
	}
	return roleFromRow(r)
}

func (s roles) List(ctx context.Context) ([]*directory.Role, error) {
	rows, err := gen.New(s.db).ListRoles(ctx)
	if err != nil {
		return nil, mapErr("list roles", err)
	}
	return convert(rows, roleFromRow)
}

func (s roles) Upsert(ctx context.Context, r *directory.Role) error {
	md, err := encodeMetadata(r.Meta)
	if err != nil {
		return err
	}
	doc := roleSpecDoc{Enabled: r.Spec.Enabled}
	if r.Spec.Rules != nil {
		doc.Rules = make([]ruleDoc, len(r.Spec.Rules))
		for i, rule := range r.Spec.Rules {
			doc.Rules[i] = ruleDoc{Kinds: rule.Kinds, Verbs: rule.Verbs}
		}
	}
	spec, err := encodeSpec(doc, r.Spec.Attributes, roleSpecKeys)
	if err != nil {
		return err
	}
	ver, err := expectedVersion(r.Meta)
	if err != nil {
		return err
	}
	return mapErr("upsert role", staleIfNoRows(gen.New(s.db).UpsertRole(ctx, gen.UpsertRoleParams{
		ID: r.Meta.ID, Name: r.Meta.Name, DisplayName: r.Meta.DisplayName,
		Metadata: md, Spec: spec, ExpectedVersion: ver,
	})))
}

func (s roles) Delete(ctx context.Context, id string) error {
	return mapErr("delete role", gen.New(s.db).DeleteRole(ctx, id))
}

func roleFromRow(r gen.Role) (*directory.Role, error) {
	m, err := decodeMetadata(r.ID, r.Name, r.DisplayName, r.Metadata, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
	if err != nil {
		return nil, err
	}
	var doc roleSpecDoc
	attrs, err := decodeSpec(r.Spec, &doc, roleSpecKeys)
	if err != nil {
		return nil, err
	}
	var rules []rbac.Rule
	if doc.Rules != nil {
		rules = make([]rbac.Rule, len(doc.Rules))
		for i, rule := range doc.Rules {
			rules[i] = rbac.Rule{Kinds: rule.Kinds, Verbs: rule.Verbs}
		}
	}
	return &directory.Role{Meta: m, Spec: directory.RoleSpec{Rules: rules, Enabled: doc.Enabled, Attributes: attrs}}, nil
}
