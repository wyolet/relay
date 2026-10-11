package directory

import (
	"context"
	"fmt"
)

func teamMeta(t *Team) *Metadata       { return &t.Meta }
func projectMeta(p *Project) *Metadata { return &p.Meta }

// CreateTeam adds a team. An empty owner is the system.
func (d *Directory) CreateTeam(ctx context.Context, t *Team) error {
	defaultOwner(&t.Meta)
	if err := prepareCreate(ctx, d.cfg.Stores.Teams, "team", &t.Meta, teamMeta); err != nil {
		return err
	}
	return d.writeTeam(ctx, t)
}

// UpdateTeam replaces the team with id t.Meta.ID.
func (d *Directory) UpdateTeam(ctx context.Context, t *Team) error {
	if _, err := loadForUpdate(ctx, d.cfg.Stores.Teams, "team", &t.Meta, teamMeta); err != nil {
		return err
	}
	defaultOwner(&t.Meta)
	return d.writeTeam(ctx, t)
}

// DeleteTeam removes the team; its store takes the team's projects and their service accounts with it.
func (d *Directory) DeleteTeam(ctx context.Context, id string) error {
	return deleteRow(ctx, d, d.cfg.Stores.Teams, KindTeam, id)
}

func (d *Directory) writeTeam(ctx context.Context, t *Team) error {
	if err := t.validate(); err != nil {
		return err
	}
	if err := d.cfg.Stores.Teams.Upsert(ctx, t); err != nil {
		return err
	}
	d.notify(ctx, KindTeam, OpUpsert, t.Meta.ID)
	return nil
}

// CreateProject adds a project to the team named by its spec.
func (d *Directory) CreateProject(ctx context.Context, p *Project) error {
	if err := prepareCreate(ctx, d.cfg.Stores.Projects, "project", &p.Meta, projectMeta); err != nil {
		return err
	}
	return d.writeProject(ctx, p)
}

// UpdateProject replaces the project with id p.Meta.ID; changing TeamID moves it to another team.
func (d *Directory) UpdateProject(ctx context.Context, p *Project) error {
	if _, err := loadForUpdate(ctx, d.cfg.Stores.Projects, "project", &p.Meta, projectMeta); err != nil {
		return err
	}
	return d.writeProject(ctx, p)
}

// DeleteProject removes the project; its store takes the project's service accounts with it.
func (d *Directory) DeleteProject(ctx context.Context, id string) error {
	return deleteRow(ctx, d, d.cfg.Stores.Projects, KindProject, id)
}

func (d *Directory) writeProject(ctx context.Context, p *Project) error {
	p.StampOwner()
	if err := p.validate(); err != nil {
		return err
	}
	t, err := d.cfg.Stores.Teams.Get(ctx, p.Spec.TeamID)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("%w: project %q: team %q does not exist", ErrInvalid, p.Meta.Name, p.Spec.TeamID)
	}
	if err := d.cfg.Stores.Projects.Upsert(ctx, p); err != nil {
		return err
	}
	d.notify(ctx, KindProject, OpUpsert, p.Meta.ID)
	return nil
}
