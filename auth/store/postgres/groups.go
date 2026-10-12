// Members live in group_members, not the spec document, so the foreign key to users drops a deleted user's memberships. position keeps the order they were given in.

package postgres

import (
	"context"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

type groups struct{ db DB }

func (s groups) Get(ctx context.Context, id string) (*directory.Group, error) {
	q := gen.New(s.db)
	r, err := q.GetGroup(ctx, id)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("get group", err)
	}
	g, err := groupFromRow(r)
	if err != nil {
		return nil, err
	}
	members, err := q.GetGroupMembers(ctx, id)
	if err != nil {
		return nil, mapErr("get group members", err)
	}
	for _, m := range members {
		g.Spec.MemberIDs = append(g.Spec.MemberIDs, m.UserID)
	}
	return g, nil
}

func (s groups) List(ctx context.Context) ([]*directory.Group, error) {
	q := gen.New(s.db)
	rows, err := q.ListGroups(ctx)
	if err != nil {
		return nil, mapErr("list groups", err)
	}
	members, err := q.ListGroupMembers(ctx)
	if err != nil {
		return nil, mapErr("list group members", err)
	}
	byGroup := map[string][]string{}
	for _, m := range members {
		byGroup[m.GroupID] = append(byGroup[m.GroupID], m.UserID)
	}
	out, err := convert(rows, groupFromRow)
	if err != nil {
		return nil, err
	}
	for _, g := range out {
		g.Spec.MemberIDs = byGroup[g.Meta.ID]
	}
	return out, nil
}

func (s groups) Upsert(ctx context.Context, g *directory.Group) error {
	md, err := encodeMetadata(g.Meta)
	if err != nil {
		return err
	}
	spec, err := encodeSpec(enabledDoc{Enabled: g.Spec.Enabled}, g.Spec.Attributes, groupSpecKeys)
	if err != nil {
		return err
	}
	ver, err := expectedVersion(g.Meta)
	if err != nil {
		return err
	}
	return inTx(ctx, s.db, func(q *gen.Queries) error {
		if err := mapErr("upsert group", staleIfNoRows(q.UpsertGroup(ctx, gen.UpsertGroupParams{
			ID: g.Meta.ID, Name: g.Meta.Name, DisplayName: g.Meta.DisplayName,
			Metadata: md, Spec: spec, ExpectedVersion: ver,
			ExternalSource: text(g.Spec.External.Source), ExternalID: text(g.Spec.External.ID),
		}))); err != nil {
			return err
		}
		if err := q.DeleteGroupMembers(ctx, g.Meta.ID); err != nil {
			return mapErr("clear group members", err)
		}
		for i, id := range g.Spec.MemberIDs {
			if err := q.InsertGroupMember(ctx, gen.InsertGroupMemberParams{
				GroupID: g.Meta.ID, UserID: id, Position: int32(i),
			}); err != nil {
				return mapErr("insert group member", err)
			}
		}
		return nil
	})
}

func (s groups) Delete(ctx context.Context, id string) error {
	return mapErr("delete group", gen.New(s.db).DeleteGroup(ctx, id))
}

func groupFromRow(r gen.Group) (*directory.Group, error) {
	m, err := decodeMetadata(r.ID, r.Name, r.DisplayName, r.Metadata, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
	if err != nil {
		return nil, err
	}
	var doc enabledDoc
	attrs, err := decodeSpec(r.Spec, &doc, groupSpecKeys)
	if err != nil {
		return nil, err
	}
	return &directory.Group{Meta: m, Spec: directory.GroupSpec{
		Enabled:    doc.Enabled,
		External:   directory.External{Source: r.ExternalSource.String, ID: r.ExternalID.String},
		Attributes: attrs,
	}}, nil
}
