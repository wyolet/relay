package apply

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/settings/settingstest"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// planOne indexes b's rows and plans w's documents through planKind, with
// writes that record the row they would persist instead of storing it.
// written fills as the caller runs the entries' writes; Delete is a no-op.
func planOne[D, T any](ctx context.Context, b *builder, w kindWiring[D, T]) (written *[]*T, err error) {
	written = new([]*T)
	b.idx = newIndex(b.rows)
	w.Upsert = func(_ context.Context, x *T) error {
		*written = append(*written, x)
		return nil
	}
	w.Delete = func(context.Context, string) error { return nil }
	return written, planKind(ctx, b, w)
}

// runWrites runs every write the plan in b holds.
func runWrites(t *testing.T, b *builder) {
	t.Helper()
	for _, e := range b.entries {
		if e.write != nil {
			if err := e.write(context.Background()); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
}

// teamPlan runs one Team document set through planKind against the given
// stored rows, and hands back the builder so the caller can read the entries
// and the row each write would persist.
func teamPlan(t *testing.T, b *builder, docs ...*manifest.TeamDTO) ([]Entry, []*team.Team, error) {
	t.Helper()
	names := map[string]string{}
	for _, row := range b.rows.Teams {
		names[row.Meta.Name] = row.Meta.ID
	}
	written, err := planOne(context.Background(), b, kindWiring[manifest.TeamDTO, team.Team]{
		Kind: "Team", Docs: docs, Names: names, Rows: b.rows.Teams,
		To:   manifest.ToTeam,
		Meta: func(x *team.Team) *meta.Metadata { return &x.Meta },
	})
	if err == nil {
		runWrites(t, b)
	}
	return b.entries, *written, err
}

// rateLimitPlan is teamPlan for a kind whose owner the document authors —
// Team, Group and Role are system-owned by rule, so they cannot exercise the
// owner guards.
func rateLimitPlan(t *testing.T, b *builder, docs ...*manifest.RateLimitDTO) ([]Entry, []*ratelimit.RateLimit, error) {
	t.Helper()
	names := map[string]string{}
	for _, row := range b.rows.RateLimits {
		names[row.Meta.Name] = row.Meta.ID
	}
	written, err := planOne(context.Background(), b, kindWiring[manifest.RateLimitDTO, ratelimit.RateLimit]{
		Kind: "RateLimit", Docs: docs, Names: names, Rows: b.rows.RateLimits,
		To:   manifest.ToRateLimit,
		Meta: func(x *ratelimit.RateLimit) *meta.Metadata { return &x.Meta },
	})
	if err == nil {
		runWrites(t, b)
	}
	return b.entries, *written, err
}

func rateLimitDoc(name string) *manifest.RateLimitDTO {
	d := &manifest.RateLimitDTO{APIVersion: manifest.APIVersion, Kind: "RateLimit"}
	d.Metadata.Name = name
	d.Metadata.DisplayName = name
	d.Spec.Rules = []manifest.RateLimitRule{{Meter: "requests", Amount: 10, Window: "1m", Strategy: "token-bucket"}}
	return d
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
	storedRow := func() *ratelimit.RateLimit {
		return &ratelimit.RateLimit{
			Meta: meta.Metadata{
				ID: meta.NewID(), Name: "burst", DisplayName: "burst",
				Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"},
			},
			Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
				Meter: "requests", Amount: 10, Window: ratelimit.Window(time.Minute),
				Strategy: "token-bucket",
			}}},
		}
	}
	doc := rateLimitDoc("burst")
	doc.Metadata.Owner.Kind = meta.OwnerUser
	doc.Metadata.Owner.ID = "u-mallory"

	stored := storedRow()
	b := &builder{rows: &Rows{RateLimits: []*ratelimit.RateLimit{stored}}, lic: license.Community}
	entries, written, err := rateLimitPlan(t, b, doc)
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
	b = &builder{rows: &Rows{RateLimits: []*ratelimit.RateLimit{storedRow()}}, admin: true, lic: license.Community}
	entries, written, err = rateLimitPlan(t, b, doc)
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
	doc := rateLimitDoc("burst")
	doc.Metadata.Owner.Kind = meta.OwnerProvider // a provider owner needs an id
	b := &builder{rows: &Rows{}, lic: license.Community}
	_, written, err := rateLimitPlan(t, b, doc)
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("err = %v, want an InvalidError", err)
	}
	if invalid.Name != "burst" || len(written) != 0 {
		t.Fatalf("invalid row named %q, written %d", invalid.Name, len(written))
	}
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
	locked := Options{Gov: settingstest.Sections(map[string]any{
		settings.SectionGovernanceModel: &settings.Governance{AllowEdit: false},
	})}

	plan := func(opts Options) error {
		b := &builder{opts: opts, rows: &Rows{Models: []*model.Model{stored}}}
		d := &manifest.ModelDTO{APIVersion: manifest.APIVersion, Kind: "Model"}
		d.Metadata.Name = "gpt-4o"
		d.Metadata.DisplayName = "GPT-4o mini"
		d.Metadata.Owner.Kind = meta.OwnerProvider
		d.Metadata.Owner.ID = providerID
		d.Spec.Snapshots = []model.Snapshot{{Name: "gpt-4o"}}
		d.Spec.Pointer = "gpt-4o"
		_, err := planOne(context.Background(), b, kindWiring[manifest.ModelDTO, model.Model]{
			Kind: "Model", Docs: []*manifest.ModelDTO{d},
			Names: map[string]string{"gpt-4o": stored.Meta.ID}, Rows: b.rows.Models,
			To:   manifest.ToModel,
			Meta: func(x *model.Model) *meta.Metadata { return &x.Meta },
		})
		return err
	}

	var gov *GovernanceError
	if err := plan(locked); !errors.As(err, &gov) || gov.Kind != "Model" || gov.Name != "gpt-4o" {
		t.Fatalf("locked edit = %v, want a GovernanceError on Model gpt-4o", err)
	}
	if err := plan(Options{}); err != nil {
		t.Fatalf("without a governance reader the plan must stand: %v", err)
	}
}

// dirtyRateLimit is the stored row rateLimitDoc("burst") declares, marked as edited through the control API.
func dirtyRateLimit() *ratelimit.RateLimit {
	return &ratelimit.RateLimit{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "burst", DisplayName: "burst", Dirty: true,
			Owner: meta.Owner{Kind: meta.OwnerUser, ID: "u-alice"},
		},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: "requests", Amount: 10, Window: ratelimit.Window(time.Minute),
			Strategy: "token-bucket",
		}}},
	}
}

// A dirty row is an operator's edit: one equal to the document is not drift,
// one that differs is reported and left alone, and force resets it.
func TestApplyDirtyRows(t *testing.T) {
	renamed := rateLimitDoc("burst")
	renamed.Metadata.DisplayName = "Burst (renamed)"
	for _, tc := range []struct {
		name       string
		doc        *manifest.RateLimitDTO
		force      bool
		wantAction Action
		wantFields []string
		wantWrite  bool
	}{
		{"equal to the document", rateLimitDoc("burst"), false, ActionUnchanged, nil, false},
		{"equal to the document, forced", rateLimitDoc("burst"), true, ActionUnchanged, nil, false},
		{"differs", renamed, false, ActionSkipDirty, []string{"metadata.displayName"}, false},
		{"differs, forced", renamed, true, ActionUpdate, []string{"metadata.displayName"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &builder{opts: Options{Force: tc.force}, rows: &Rows{RateLimits: []*ratelimit.RateLimit{dirtyRateLimit()}}, lic: license.Community}
			entries, written, err := rateLimitPlan(t, b, tc.doc)
			if err != nil {
				t.Fatalf("planKind: %v", err)
			}
			if len(entries) != 1 || entries[0].Action != tc.wantAction || !slices.Equal(entries[0].ChangedFields, tc.wantFields) {
				t.Fatalf("entries = %+v, want one %s row changing %v", entries, tc.wantAction, tc.wantFields)
			}
			if !tc.wantWrite {
				if len(written) != 0 {
					t.Fatalf("a dirty row was written: %+v", written[0])
				}
				return
			}
			if len(written) != 1 || written[0].Meta.DisplayName != "Burst (renamed)" || written[0].Meta.Dirty {
				t.Fatalf("forced write = %+v, want the document's row with the dirty flag cleared", written)
			}
		})
	}
}

// A user owner with no id names nobody. Applied by a caller that is no user
// (the admin token, the boot seed) the row is shared infrastructure and
// lands organization-owned; applied by a user it becomes theirs, as on CRUD.
func TestApplyStampsAnIDlessUserOwner(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want meta.Owner
	}{
		{"admin token", actor.WithActor(context.Background(), &actor.Actor{AdminToken: true}), meta.Global},
		{"boot seed", context.Background(), meta.Global},
		{"user session", actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"}), meta.Owner{Kind: meta.OwnerUser, ID: "u-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &builder{rows: &Rows{}, admin: true}
			d := rateLimitDoc("ci-limit")
			d.Metadata.Owner.Kind = meta.OwnerUser
			written, err := planOne(tc.ctx, b, kindWiring[manifest.RateLimitDTO, ratelimit.RateLimit]{
				Kind: "RateLimit", Docs: []*manifest.RateLimitDTO{d},
				Names: map[string]string{"ci-limit": meta.NewID()},
				To:    manifest.ToRateLimit,
				Meta:  func(x *ratelimit.RateLimit) *meta.Metadata { return &x.Meta },
			})
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			for _, e := range b.entries {
				if e.owner != tc.want {
					t.Fatalf("entry owner = %+v, want %+v", e.owner, tc.want)
				}
				if err := e.write(tc.ctx); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if len(*written) != 1 || (*written)[0].Meta.Owner != tc.want {
				t.Fatalf("written = %+v, want one row owned by %+v", *written, tc.want)
			}
		})
	}
}

func hostDoc(name string, owner manifest.WireOwner) *manifest.HostDTO {
	d := &manifest.HostDTO{APIVersion: manifest.APIVersion, Kind: "Host"}
	d.Metadata.Name = name
	d.Metadata.Owner = owner
	d.Spec.BaseURL = "https://upstream.example"
	return d
}

func planHost(ctx context.Context, admin bool, d *manifest.HostDTO) ([]Entry, error) {
	b := &builder{rows: &Rows{}, admin: admin}
	_, err := planOne(ctx, b, kindWiring[manifest.HostDTO, host.Host]{
		Kind: "Host", Docs: []*manifest.HostDTO{d},
		Names: map[string]string{d.Metadata.Name: meta.NewID()},
		To:    manifest.ToHost,
		Meta:  func(x *host.Host) *meta.Metadata { return &x.Meta },
	})
	return b.entries, err
}

// The catalog's own documents mark an operator-configured host with an
// id-less user owner. Whoever applies one, the host lands system-owned:
// a host is never anyone's personal row.
func TestApplyHostWithIDlessUserOwnerIsSystemOwned(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		admin bool
	}{
		{"boot seed", context.Background(), true},
		{"admin token", actor.WithActor(context.Background(), &actor.Actor{AdminToken: true}), true},
		{"admin session", actor.WithActor(context.Background(), &actor.Actor{UserID: "u-root", Roles: []string{user.RoleAdmin}}), true},
		{"user session", actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := planHost(tc.ctx, tc.admin, hostDoc("ollama-self", manifest.WireOwner{Kind: meta.OwnerUser}))
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			if len(entries) != 1 || entries[0].owner != (meta.Owner{Kind: meta.OwnerSystem}) {
				t.Fatalf("entries = %+v, want one system-owned host", entries)
			}
		})
	}
}

// A document naming a real tenant owner for a host is refused, for the
// admin as for anyone: the rule is about the row, not the caller.
func TestApplyRefusesTenantOwnedHost(t *testing.T) {
	session := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	for _, owner := range []manifest.WireOwner{
		{Kind: meta.OwnerUser, ID: "u-1"},
		{Kind: meta.OwnerTeam, ID: meta.NewID()},
		{Kind: meta.OwnerProject, ID: meta.NewID()},
	} {
		for _, admin := range []bool{false, true} {
			entries, err := planHost(session, admin, hostDoc("box", owner))
			want := fmt.Sprintf(`apply: Host "box": host is shared catalog data and cannot be owned by a %s`, owner.Kind)
			if err == nil || err.Error() != want || len(entries) != 0 {
				t.Fatalf("owner %+v admin=%v: err = %v (entries %d), want %q", owner, admin, err, len(entries), want)
			}
		}
	}
}

// A HostBinding document follows the same rule.
func TestApplyRefusesTenantOwnedHostBinding(t *testing.T) {
	session := actor.WithActor(context.Background(), &actor.Actor{UserID: "u-1"})
	for _, owner := range []manifest.WireOwner{
		{Kind: meta.OwnerUser, ID: "u-1"},
		{Kind: meta.OwnerProject, ID: meta.NewID()},
	} {
		b := &builder{rows: &Rows{}}
		d := &manifest.HostBindingDTO{APIVersion: manifest.APIVersion, Kind: "HostBinding"}
		d.Metadata.Name = "m-on-box"
		d.Metadata.Owner = owner
		_, err := planOne(session, b, kindWiring[manifest.HostBindingDTO, binding.Binding]{
			Kind: "HostBinding", Docs: []*manifest.HostBindingDTO{d},
			Names: map[string]string{d.Metadata.Name: meta.NewID()},
			// The owner rule is what is under test, not name resolution.
			To: func(d manifest.HostBindingDTO, _ manifest.Resolver) (*binding.Binding, error) {
				return &binding.Binding{
					Meta: meta.Metadata{Name: d.Metadata.Name, Owner: meta.Owner{Kind: d.Metadata.Owner.Kind, ID: d.Metadata.Owner.ID}},
					Spec: binding.Spec{ModelID: meta.NewID(), HostID: meta.NewID()},
				}, nil
			},
			Meta: func(x *binding.Binding) *meta.Metadata { return &x.Meta },
		})
		want := fmt.Sprintf(`apply: HostBinding "m-on-box": host-binding is shared catalog data and cannot be owned by a %s`, owner.Kind)
		if err == nil || err.Error() != want || len(b.entries) != 0 {
			t.Fatalf("owner %+v: err = %v (entries %d), want %q", owner, err, len(b.entries), want)
		}
	}
}

// detachedModel is a provider-owned catalog row a catalog upgrade dropped.
func detachedModel(providerID string, at time.Time) *model.Model {
	return &model.Model{
		Meta: meta.Metadata{
			ID: meta.NewID(), Name: "gpt-4o", DisplayName: "GPT-4o",
			Owner:             meta.Owner{Kind: meta.OwnerProvider, ID: providerID},
			CatalogDetachedAt: &at,
		},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: "gpt-4o"}}, Pointer: "gpt-4o"},
	}
}

func modelDoc(providerID, displayName string) *manifest.ModelDTO {
	d := &manifest.ModelDTO{APIVersion: manifest.APIVersion, Kind: "Model"}
	d.Metadata.Name = "gpt-4o"
	d.Metadata.DisplayName = displayName
	d.Metadata.Owner.Kind = meta.OwnerProvider
	d.Metadata.Owner.ID = providerID
	d.Spec.Snapshots = []model.Snapshot{{Name: "gpt-4o"}}
	d.Spec.Pointer = "gpt-4o"
	return d
}

func planModel(t *testing.T, b *builder, stored *model.Model, doc *manifest.ModelDTO) ([]*model.Model, error) {
	t.Helper()
	written, err := planOne(context.Background(), b, kindWiring[manifest.ModelDTO, model.Model]{
		Kind: "Model", Docs: []*manifest.ModelDTO{doc},
		Names: map[string]string{stored.Meta.Name: stored.Meta.ID}, Rows: b.rows.Models,
		To:   manifest.ToModel,
		Meta: func(x *model.Model) *meta.Metadata { return &x.Meta },
	})
	if err == nil {
		runWrites(t, b)
	}
	return *written, err
}

// An operator applying a detached row's document neither re-attaches it nor
// sees the marker as drift: only the catalog reseed moves it.
func TestApplyKeepsCatalogDetachedAtOutOfTheDiff(t *testing.T) {
	providerID := meta.NewID()
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	stored := detachedModel(providerID, at)

	b := &builder{rows: &Rows{Models: []*model.Model{stored}}, lic: license.Community}
	written, err := planModel(t, b, stored, modelDoc(providerID, "GPT-4o"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(b.entries) != 1 || b.entries[0].Action != ActionUnchanged || len(written) != 0 {
		t.Fatalf("entries = %+v, written %d; want one unchanged row and no write", b.entries, len(written))
	}

	b = &builder{rows: &Rows{Models: []*model.Model{stored}}, lic: license.Community}
	written, err = planModel(t, b, stored, modelDoc(providerID, "GPT-4o (renamed)"))
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(b.entries) != 1 || b.entries[0].Action != ActionUpdate || len(written) != 1 {
		t.Fatalf("entries = %+v, written %d; want one update", b.entries, len(written))
	}
	for _, f := range b.entries[0].ChangedFields {
		if f == "metadata.catalogDetachedAt" {
			t.Fatalf("the marker reached the diff: %v", b.entries[0].ChangedFields)
		}
	}
	if got := written[0].Meta.CatalogDetachedAt; got == nil || !got.Equal(at) {
		t.Fatalf("written marker = %v, want the stored %v", got, at)
	}
}

// A detached catalog row is the operator's, so the governance section that
// locks catalog rows no longer applies to it.
func TestApplyGovernsADetachedRowAsUserOwned(t *testing.T) {
	providerID := meta.NewID()
	stored := detachedModel(providerID, time.Now())
	locked := Options{Gov: settingstest.Sections(map[string]any{
		settings.SectionGovernanceModel: &settings.Governance{AllowEdit: false},
	})}

	b := &builder{opts: locked, rows: &Rows{Models: []*model.Model{stored}}, lic: license.Community}
	if _, err := planModel(t, b, stored, modelDoc(providerID, "GPT-4o (renamed)")); err != nil {
		t.Fatalf("edit of a detached row under a locked section = %v, want it allowed", err)
	}

	attached := detachedModel(providerID, time.Now())
	attached.Meta.CatalogDetachedAt = nil
	b = &builder{opts: locked, rows: &Rows{Models: []*model.Model{attached}}, lic: license.Community}
	var gov *GovernanceError
	if written, err := planModel(t, b, attached, modelDoc(providerID, "GPT-4o (renamed)")); !errors.As(err, &gov) || gov.Name != "gpt-4o" || len(written) != 0 {
		t.Fatalf("edit of an attached row under a locked section = %v (written %d), want a GovernanceError", err, len(written))
	}
}

// seesOnly is a scoping authorizer that allows every verb on the rows it
// names by id, and nothing else.
type seesOnly map[string]bool

func (s seesOnly) Authorize(_ context.Context, _ string, res authz.Resource) error {
	if s[res.ID] {
		return nil
	}
	return authz.ErrForbidden
}

func (s seesOnly) Visible(ctx context.Context, kind, id string, owner meta.Owner) bool {
	return s.Authorize(ctx, kind+".get", authz.Resource{Kind: kind, ID: id, Owner: &owner}) == nil
}

func hiddenPlanBuilder(rows *Rows, a authz.Authorizer, prune bool) *builder {
	b := &builder{opts: Options{Stores: &Stores{}, Authz: a}, rows: rows, idx: newIndex(rows)}
	if prune {
		b.opts.Prune, b.opts.Selector = true, "env=prod"
		b.selector = labelSelector{"env": "prod"}
	}
	return b
}

// tenantRows is team t > project p (both env=prod) > service accounts.
func tenantRows(accounts ...string) (*Rows, *team.Team, *project.Project) {
	lbl := map[string]string{"env": "prod"}
	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "t", Owner: meta.Owner{Kind: meta.OwnerSystem}, Labels: lbl}}
	p := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "p", Labels: lbl}, Spec: project.Spec{TeamID: tm.Meta.ID}}
	p.StampOwner()
	rows := &Rows{Teams: []*team.Team{tm}, Projects: []*project.Project{p}}
	for _, name := range accounts {
		sa := &serviceaccount.ServiceAccount{Meta: meta.Metadata{ID: meta.NewID(), Name: name}, Spec: serviceaccount.Spec{ProjectID: p.Meta.ID}}
		sa.StampOwner()
		rows.ServiceAccounts = append(rows.ServiceAccounts, sa)
	}
	return rows, tm, p
}

func TestPlanPruneSkipsRowsTheCallerCannotSee(t *testing.T) {
	rows, _, _ := tenantRows("billing-bot")
	b := hiddenPlanBuilder(rows, seesOnly{}, true)
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("plan = %v, want no error", err)
	}
	if len(b.entries) != 0 {
		t.Fatalf("plan touches rows the caller cannot see: %+v", b.entries)
	}

	// The same rows are all pruned, children first, once the caller sees them.
	rows, tm, p := tenantRows("billing-bot")
	rows.ServiceAccounts[0].Meta.Labels = p.Meta.Labels
	b = hiddenPlanBuilder(rows, seesOnly{tm.Meta.ID: true, p.Meta.ID: true, rows.ServiceAccounts[0].Meta.ID: true}, true)
	if err := b.run(context.Background(), nil); err != nil {
		t.Fatalf("plan = %v, want no error", err)
	}
	var deleted []string
	for _, e := range b.entries {
		deleted = append(deleted, string(e.Action)+" "+e.Kind+"/"+e.Name)
	}
	if want := []string{"delete ServiceAccount/billing-bot", "delete Project/p", "delete Team/t"}; !slices.Equal(deleted, want) {
		t.Fatalf("plan = %v, want %v", deleted, want)
	}
}

func TestPlanPruneNamesOnlyVisibleDependents(t *testing.T) {
	rows, _, p := tenantRows("seen-bot", "hidden-bot")
	rows.Teams = nil
	b := hiddenPlanBuilder(rows, seesOnly{p.Meta.ID: true, rows.ServiceAccounts[0].Meta.ID: true}, true)
	err := b.run(context.Background(), nil)
	var de *project.DependentsError
	if !errors.As(err, &de) {
		t.Fatalf("plan = %v, want a dependents error for the visible project", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "seen-bot") || strings.Contains(msg, "hidden-bot") || !strings.Contains(msg, "1 more") {
		t.Fatalf("dependents error = %q, want seen-bot named and hidden-bot only counted", msg)
	}
}

func groupDoc(member string) manifest.Document {
	d := &manifest.GroupDTO{APIVersion: manifest.APIVersion, Kind: "Group"}
	d.Metadata.Name = "probe"
	d.Spec.Members = []string{member}
	return manifest.Document{Group: d}
}

// An unresolved name is typed so the API can answer it like a hidden row.
func TestPlanUnresolvedNameIsRefNotFound(t *testing.T) {
	rows := &Rows{Users: []*user.User{{ID: meta.NewID(), Username: "alice@example.test"}}}
	err := hiddenPlanBuilder(rows, seesOnly{}, false).run(context.Background(), []manifest.Document{groupDoc("bob@example.test")})
	var nf *manifest.RefNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("plan = %v, want a RefNotFoundError", err)
	}
	if err := hiddenPlanBuilder(rows, seesOnly{}, false).run(context.Background(), []manifest.Document{groupDoc("alice@example.test")}); err != nil {
		t.Fatalf("known user: plan = %v, want success up to authorization", err)
	}
}

func serviceAccountDoc(name, projectName string) manifest.Document {
	d := &manifest.ServiceAccountDTO{APIVersion: manifest.APIVersion, Kind: "ServiceAccount"}
	d.Metadata.Name = name
	d.Spec.Project = projectName
	return manifest.Document{ServiceAccount: d}
}

// Re-declaring a hidden row's name is refused before validation or the
// reference checks can describe the stored row.
func TestPlanRedeclaringHiddenRowIsForbidden(t *testing.T) {
	rows, _, theirs := tenantRows("shared-name")
	mine := &project.Project{Meta: meta.Metadata{ID: meta.NewID(), Name: "mine"}, Spec: project.Spec{TeamID: meta.NewID()}}
	mine.StampOwner()
	rows.Projects = append(rows.Projects, mine)

	err := hiddenPlanBuilder(rows, seesOnly{mine.Meta.ID: true}, false).run(context.Background(), []manifest.Document{serviceAccountDoc("shared-name", "mine")})
	var ae *AuthzError
	if !errors.As(err, &ae) {
		t.Fatalf("plan = %v, want an AuthzError", err)
	}
	if strings.Contains(err.Error(), theirs.Meta.ID) {
		t.Fatalf("error names the hidden owner: %v", err)
	}

	// A caller who sees the row still gets the specific reason.
	seen := rows.ServiceAccounts[0].Meta.ID
	err = hiddenPlanBuilder(rows, seesOnly{mine.Meta.ID: true, seen: true}, false).run(context.Background(), []manifest.Document{serviceAccountDoc("shared-name", "mine")})
	var inv *InvalidError
	if !errors.As(err, &inv) || inv.Kind != "ServiceAccount" || inv.Name != "shared-name" {
		t.Fatalf("visible row: plan = %v, want an InvalidError on ServiceAccount shared-name", err)
	}
}
