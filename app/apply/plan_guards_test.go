package apply

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/team"
)

// teamPlan runs one Team document set through planKind against the given
// stored rows, and hands back the builder so the caller can read the entries
// and the row each write would persist.
func teamPlan(t *testing.T, b *builder, docs ...*manifest.TeamDTO) ([]Entry, []*team.Team, error) {
	t.Helper()
	var written []*team.Team
	names := map[string]string{}
	for _, row := range b.rows.Teams {
		names[row.Meta.Name] = row.Meta.ID
	}
	b.idx = newIndex(b.rows)
	err := planKind(b, kindWiring[manifest.TeamDTO, team.Team]{
		Kind: "Team", Docs: docs, Names: names, Rows: b.rows.Teams,
		To:   manifest.ToTeam,
		Meta: func(x *team.Team) *meta.Metadata { return &x.Meta },
		Upsert: func(_ context.Context, x *team.Team) error {
			written = append(written, x)
			return nil
		},
		Delete: func(context.Context, string) error { return nil },
	})
	if err != nil {
		return b.entries, written, err
	}
	for _, e := range b.entries {
		if e.write != nil {
			if werr := e.write(context.Background()); werr != nil {
				t.Fatalf("write: %v", werr)
			}
		}
	}
	return b.entries, written, err
}

func teamDoc(name string) *manifest.TeamDTO {
	d := &manifest.TeamDTO{APIVersion: manifest.APIVersion, Kind: "Team"}
	d.Metadata.Name = name
	d.Metadata.DisplayName = name
	return d
}

// The stored owner is what the row keeps: a document that names a different
// one would otherwise move the row into another tenant's scope on every
// apply of a repo anyone can open a PR against.
func TestApplyKeepsTheStoredOwner(t *testing.T) {
	stored := &team.Team{Meta: meta.Metadata{
		ID: meta.NewID(), Name: "platform", DisplayName: "platform",
		Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"},
	}}
	doc := teamDoc("platform")
	doc.Metadata.Owner.Kind = meta.OwnerUser
	doc.Metadata.Owner.ID = "u-mallory"

	b := &builder{rows: &Rows{Teams: []*team.Team{stored}}}
	entries, written, err := teamPlan(t, b, doc)
	if err != nil {
		t.Fatalf("planKind: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != ActionUnchanged {
		t.Fatalf("entries = %+v, want one unchanged row", entries)
	}
	if len(written) != 0 {
		t.Fatalf("an owner-only difference must not be written: %+v", written[0].Meta.Owner)
	}

	// The admin identity may re-parent deliberately.
	b = &builder{rows: &Rows{Teams: []*team.Team{stored}}, admin: true}
	entries, written, err = teamPlan(t, b, doc)
	if err != nil {
		t.Fatalf("planKind (admin): %v", err)
	}
	if len(entries) != 1 || entries[0].Action != ActionUpdate {
		t.Fatalf("admin entries = %+v, want an update", entries)
	}
	if len(written) != 1 || written[0].Meta.Owner.ID != "u-mallory" {
		t.Fatalf("admin write = %+v, want the document's owner", written)
	}
}

// Two documents naming the same row is a mistake in the repo, not an
// instruction to keep the last one.
func TestApplyRejectsDuplicateNames(t *testing.T) {
	b := &builder{rows: &Rows{}}
	_, _, err := teamPlan(t, b, teamDoc("platform"), teamDoc("platform"))
	var dup *DuplicateError
	if !errors.As(err, &dup) {
		t.Fatalf("err = %v, want a DuplicateError", err)
	}
	if dup.Kind != "Team" || dup.Name != "platform" {
		t.Fatalf("duplicate reported as %s %q", dup.Kind, dup.Name)
	}
}

// A document the entity itself rejects must never reach the store: a bad row
// in Postgres breaks the next Bootstrap.
func TestApplyValidatesEveryPlannedRow(t *testing.T) {
	doc := teamDoc("platform")
	doc.Metadata.Owner.Kind = meta.OwnerProvider // Team allows user or system only
	b := &builder{rows: &Rows{}}
	_, written, err := teamPlan(t, b, doc)
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want an InvalidError", err)
	}
	if invalid.Name != "platform" || len(written) != 0 {
		t.Fatalf("invalid row named %q, written %d", invalid.Name, len(written))
	}
}

// govReader answers one governance section.
type govReader struct {
	section string
	value   *settings.Governance
}

func (g govReader) Setting(name string) (any, bool) {
	if name != g.section {
		return nil, false
	}
	return g.value, true
}

// A catalog-managed row is only editable while its governance section says
// so; apply is a mutation like any other.
func TestApplyHonoursGovernance(t *testing.T) {
	providerID := meta.NewID()
	stored := &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o", DisplayName: "GPT-4o",
			Owner: meta.Owner{Kind: meta.OwnerProvider, ID: providerID},
		},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "gpt-4o"}}, Pointer: "gpt-4o"},
	}
	locked := Options{Gov: govReader{
		section: settings.SectionGovernanceModel,
		value:   &settings.Governance{AllowEdit: false},
	}}

	plan := func(opts Options) error {
		b := &builder{opts: opts, rows: &Rows{Models: []*model.Model{stored}}}
		b.idx = newIndex(b.rows)
		d := &manifest.ModelDTO{APIVersion: manifest.APIVersion, Kind: "Model"}
		d.Metadata.Name = "gpt-4o"
		d.Metadata.DisplayName = "GPT-4o mini"
		d.Metadata.Owner.Kind = meta.OwnerProvider
		d.Metadata.Owner.ID = providerID
		d.Spec.Snapshots = []model.Snapshot{{Name: "gpt-4o"}}
		d.Spec.Pointer = "gpt-4o"
		return planKind(b, kindWiring[manifest.ModelDTO, model.Model]{
			Kind: "Model", Docs: []*manifest.ModelDTO{d},
			Names: map[string]string{"gpt-4o": stored.Meta.ID}, Rows: b.rows.Models,
			To:     manifest.ToModel,
			Meta:   func(x *model.Model) *meta.Metadata { return &x.Meta },
			Upsert: func(context.Context, *model.Model) error { return nil },
			Delete: func(context.Context, string) error { return nil },
		})
	}

	var gov *GovernanceError
	if err := plan(locked); !errors.As(err, &gov) {
		t.Fatalf("locked edit = %v, want a GovernanceError", err)
	}
	if err := plan(Options{}); err != nil {
		t.Fatalf("without a governance reader the plan must stand: %v", err)
	}
}

// scopedAuthz allows every verb on rows owned by one team, and nothing else.
type scopedAuthz struct{ teamID string }

func (s scopedAuthz) Authorize(_ context.Context, _ string, res authz.Resource) error {
	if res.Owner != nil && res.Owner.Kind == meta.OwnerTeam && res.Owner.ID == s.teamID {
		return nil
	}
	return authz.ErrForbidden
}

// A dry run is a read: rows the caller may not see are dropped from the plan
// rather than echoed back, so the diff cannot be used to enumerate other
// tenants' state.
func TestAuthorizeDropsUnreadableNonWrites(t *testing.T) {
	mine := meta.Owner{Kind: meta.OwnerTeam, ID: "t-mine"}
	theirs := meta.Owner{Kind: meta.OwnerTeam, ID: "t-theirs"}
	p := &Result{Entries: []Entry{
		{Kind: "Project", Name: "ours", Action: ActionUnchanged, plural: "projects", owner: mine},
		{Kind: "Project", Name: "theirs", Action: ActionUnchanged, plural: "projects", owner: theirs},
		{Kind: "Project", Name: "theirs-dirty", Action: ActionSkipDirty, plural: "projects", owner: theirs},
	}}
	if err := Authorize(context.Background(), p, scopedAuthz{teamID: "t-mine"}); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if len(p.Entries) != 1 || p.Entries[0].Name != "ours" {
		t.Fatalf("plan = %+v, want only the caller's own row", p.Entries)
	}
	if p.Counts.Unchanged != 1 || p.Counts.SkipDirty != 0 {
		t.Fatalf("counts = %+v, want them to follow the filtered plan", p.Counts)
	}
}

// A denied write is still all-or-nothing: it aborts the run.
func TestAuthorizeStillFailsOnADeniedWrite(t *testing.T) {
	theirs := meta.Owner{Kind: meta.OwnerTeam, ID: "t-theirs"}
	p := &Result{Entries: []Entry{{
		Kind: "Project", Name: "theirs", Action: ActionUpdate, plural: "projects", owner: theirs,
		write: func(context.Context) error { return nil },
	}}}
	var ae *AuthzError
	if err := Authorize(context.Background(), p, scopedAuthz{teamID: "t-mine"}); !errors.As(err, &ae) {
		t.Fatalf("err = %v, want an AuthzError", err)
	}
}
