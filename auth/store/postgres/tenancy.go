// team_id and project_id are columns with cascading foreign keys, so deleting a team drops its projects and their service accounts without parsing JSONB. The spec documents repeat them for a reader of the JSONB alone; on read the column wins.

package postgres

import (
	"context"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

type teams struct{ db DB }

func (s teams) Get(ctx context.Context, id string) (*directory.Team, error) {
	r, err := gen.New(s.db).GetTeam(ctx, id)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("get team", err)
	}
	return teamFromRow(r)
}

func (s teams) List(ctx context.Context) ([]*directory.Team, error) {
	rows, err := gen.New(s.db).ListTeams(ctx)
	if err != nil {
		return nil, mapErr("list teams", err)
	}
	return convert(rows, teamFromRow)
}

func (s teams) Upsert(ctx context.Context, t *directory.Team) error {
	md, err := encodeMetadata(t.Meta)
	if err != nil {
		return err
	}
	spec, err := encodeSpec(enabledDoc{Enabled: t.Spec.Enabled}, t.Spec.Attributes, teamSpecKeys)
	if err != nil {
		return err
	}
	ver, err := expectedVersion(t.Meta)
	if err != nil {
		return err
	}
	return mapErr("upsert team", staleIfNoRows(gen.New(s.db).UpsertTeam(ctx, gen.UpsertTeamParams{
		ID: t.Meta.ID, Name: t.Meta.Name, DisplayName: t.Meta.DisplayName,
		Metadata: md, Spec: spec, ExpectedVersion: ver,
	})))
}

func (s teams) Delete(ctx context.Context, id string) error {
	return mapErr("delete team", gen.New(s.db).DeleteTeam(ctx, id))
}

func teamFromRow(r gen.Team) (*directory.Team, error) {
	m, err := decodeMetadata(r.ID, r.Name, r.DisplayName, r.Metadata, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
	if err != nil {
		return nil, err
	}
	var doc enabledDoc
	attrs, err := decodeSpec(r.Spec, &doc, teamSpecKeys)
	if err != nil {
		return nil, err
	}
	return &directory.Team{Meta: m, Spec: directory.TeamSpec{Enabled: doc.Enabled, Attributes: attrs}}, nil
}

type projects struct{ db DB }

func (s projects) Get(ctx context.Context, id string) (*directory.Project, error) {
	r, err := gen.New(s.db).GetProject(ctx, id)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("get project", err)
	}
	return projectFromRow(r)
}

func (s projects) List(ctx context.Context) ([]*directory.Project, error) {
	rows, err := gen.New(s.db).ListProjects(ctx)
	if err != nil {
		return nil, mapErr("list projects", err)
	}
	return convert(rows, projectFromRow)
}

func (s projects) Upsert(ctx context.Context, p *directory.Project) error {
	p.StampOwner()
	md, err := encodeMetadata(p.Meta)
	if err != nil {
		return err
	}
	spec, err := encodeSpec(projectSpecDoc{TeamID: p.Spec.TeamID, Enabled: p.Spec.Enabled}, p.Spec.Attributes, projectSpecKeys)
	if err != nil {
		return err
	}
	ver, err := expectedVersion(p.Meta)
	if err != nil {
		return err
	}
	return mapErr("upsert project", staleIfNoRows(gen.New(s.db).UpsertProject(ctx, gen.UpsertProjectParams{
		ID: p.Meta.ID, Name: p.Meta.Name, DisplayName: p.Meta.DisplayName, TeamID: p.Spec.TeamID,
		Metadata: md, Spec: spec, ExpectedVersion: ver,
	})))
}

func (s projects) Delete(ctx context.Context, id string) error {
	return mapErr("delete project", gen.New(s.db).DeleteProject(ctx, id))
}

func projectFromRow(r gen.Project) (*directory.Project, error) {
	m, err := decodeMetadata(r.ID, r.Name, r.DisplayName, r.Metadata, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
	if err != nil {
		return nil, err
	}
	var doc projectSpecDoc
	attrs, err := decodeSpec(r.Spec, &doc, projectSpecKeys)
	if err != nil {
		return nil, err
	}
	p := &directory.Project{Meta: m, Spec: directory.ProjectSpec{TeamID: r.TeamID, Enabled: doc.Enabled, Attributes: attrs}}
	p.StampOwner()
	return p, nil
}

type serviceAccounts struct{ db DB }

func (s serviceAccounts) Get(ctx context.Context, id string) (*directory.ServiceAccount, error) {
	r, err := gen.New(s.db).GetServiceAccount(ctx, id)
	if notFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, mapErr("get service account", err)
	}
	return serviceAccountFromRow(r)
}

func (s serviceAccounts) List(ctx context.Context) ([]*directory.ServiceAccount, error) {
	rows, err := gen.New(s.db).ListServiceAccounts(ctx)
	if err != nil {
		return nil, mapErr("list service accounts", err)
	}
	return convert(rows, serviceAccountFromRow)
}

func (s serviceAccounts) Upsert(ctx context.Context, sa *directory.ServiceAccount) error {
	sa.StampOwner()
	md, err := encodeMetadata(sa.Meta)
	if err != nil {
		return err
	}
	spec, err := encodeSpec(serviceAccountSpecDoc{ProjectID: sa.Spec.ProjectID, Enabled: sa.Spec.Enabled}, sa.Spec.Attributes, serviceAccountSpecKeys)
	if err != nil {
		return err
	}
	ver, err := expectedVersion(sa.Meta)
	if err != nil {
		return err
	}
	return mapErr("upsert service account", staleIfNoRows(gen.New(s.db).UpsertServiceAccount(ctx, gen.UpsertServiceAccountParams{
		ID: sa.Meta.ID, Name: sa.Meta.Name, DisplayName: sa.Meta.DisplayName, ProjectID: sa.Spec.ProjectID,
		Metadata: md, Spec: spec, ExpectedVersion: ver,
	})))
}

func (s serviceAccounts) Delete(ctx context.Context, id string) error {
	return mapErr("delete service account", gen.New(s.db).DeleteServiceAccount(ctx, id))
}

func serviceAccountFromRow(r gen.ServiceAccount) (*directory.ServiceAccount, error) {
	m, err := decodeMetadata(r.ID, r.Name, r.DisplayName, r.Metadata, r.CreatedAt, r.UpdatedAt, r.ResourceVersion)
	if err != nil {
		return nil, err
	}
	var doc serviceAccountSpecDoc
	attrs, err := decodeSpec(r.Spec, &doc, serviceAccountSpecKeys)
	if err != nil {
		return nil, err
	}
	sa := &directory.ServiceAccount{Meta: m, Spec: directory.ServiceAccountSpec{ProjectID: r.ProjectID, Enabled: doc.Enabled, Attributes: attrs}}
	sa.StampOwner()
	return sa, nil
}

// convert maps rows through from, stopping at the first error.
func convert[R, T any](rows []R, from func(R) (*T, error)) ([]*T, error) {
	out := make([]*T, 0, len(rows))
	for _, r := range rows {
		t, err := from(r)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}
