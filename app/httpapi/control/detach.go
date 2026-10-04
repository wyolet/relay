// POST /{plural}/by-id/{id}/detach releases every reference to a row that can
// let go of it — an optional field cleared, a list entry dropped — so that the
// row's DELETE can go through. It deletes nothing, edits a referencing row only
// as that row's own update would allow, and answers with what still blocks.
// Every edit lands in one transaction, or none does.
package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/internal/storage"
)

type detachOutput struct {
	Body struct {
		Detached []blockerGroup `json:"detached" doc:"References this call released, grouped like blockers."`
		Blockers []blockerGroup `json:"blockers" doc:"What still references the row. Empty means DELETE will go through."`
	}
}

// detachTarget is the row whose references are released.
type detachTarget struct {
	kind, id, name string
	owner          meta.Owner
}

// releasedRow is a referencing row detach edited: the reference it held, the
// audited action, and the fields the edit changed.
type releasedRow struct {
	item   referenceItem
	action string
	fields []string
}

func registerDetach(api huma.API, d Deps, protect huma.Middlewares) {
	for _, plural := range referencedPlurals {
		singular := authz.Singular(plural)
		huma.Register(api, huma.Operation{
			OperationID: "detach_" + singular,
			Method:      http.MethodPost,
			Path:        "/" + plural + "/by-id/{id}/detach",
			Summary:     "Release every reference to this " + singular + " that can let go",
			Description: "In one transaction, clears each optional field and drops each list entry naming the row, " +
				"on the referencing rows the caller may update. Deletes nothing; returns what still references the row.",
			Tags:        []string{plural},
			Middlewares: protect,
			Errors:      []int{401, 404, 409, 500},
		}, func(ctx context.Context, in *idInput) (*detachOutput, error) {
			return d.detach(ctx, singular, plural, in.ID)
		})
	}
}

func (d Deps) detach(ctx context.Context, kind, plural, id string) (*detachOutput, error) {
	t, err := d.findDetachTarget(ctx, kind, plural, id)
	if err != nil {
		return nil, err
	}
	var released []releasedRow
	var left []referenceItem
	err = d.Stores.InTx(ctx, func(ctx context.Context, tx *appcatalog.Stores) error {
		var err error
		released, left, err = d.boundTo(tx).releaseAll(ctx, t)
		return err
	})
	if err != nil {
		audit.Record(ctx, plural+".detach", d.auditResource(t.kind, t.id, t.name, t.owner), audit.StatusError)
		if storage.IsConflict(err) {
			return nil, &httpapi.APIError{
				Err: httpapi.APIErrorBody{
					Type:    "invalid_request_error",
					Code:    "conflict",
					Message: "another change touched these rows at the same time; nothing was detached, retry the request",
				},
				HTTPStatus: http.StatusConflict,
			}
		}
		return nil, huma.Error500InternalServerError("detach rolled back: " + err.Error())
	}
	if err := d.endSessions(ctx, released); err != nil {
		return nil, err
	}
	d.recordDetach(ctx, plural, t, released)

	items := make([]referenceItem, len(released))
	for i, r := range released {
		items[i] = r.item
	}
	out := &detachOutput{}
	out.Body.Detached = groupBlockers(ctx, d.Authz, items)
	out.Body.Blockers = groupBlockers(ctx, d.Authz, left)
	return out, nil
}

// findDetachTarget reads the row and answers 404 for one the caller may not
// see. Reading it is the only grant asked of the target: detach edits the
// referencing rows, each authorized as its own update.
func (d Deps) findDetachTarget(ctx context.Context, kind, plural, id string) (detachTarget, error) {
	notFound := huma.Error404NotFound(fmt.Sprintf("%s with id %q not found", kind, id))
	if d.Stores == nil {
		return detachTarget{}, huma.Error500InternalServerError("stores not wired")
	}
	m, err := targetMeta(ctx, d.Stores, kind, id)
	if err != nil || m == nil || !visibleTo(ctx, d.Authz, kind, id, m.Owner) {
		return detachTarget{}, notFound
	}
	if err := d.Authz.Authorize(ctx, plural+".get", authz.Resource{Kind: kind, ID: id, Name: m.Name, Owner: &m.Owner}); err != nil {
		if errors.Is(err, authz.ErrUnauthenticated) {
			return detachTarget{}, mapAuthzErr(err)
		}
		return detachTarget{}, notFound
	}
	return detachTarget{kind: kind, id: id, name: m.Name, owner: m.Owner}, nil
}

// boundTo returns d reading and writing through the transaction's stores.
func (d Deps) boundTo(tx *appcatalog.Stores) Deps {
	d.Stores = tx
	if d.Users != nil {
		d.Users = tx.Users
	}
	return d
}

// releaseAll lets go every detachable reference to t the caller may release,
// then scans again: what the second scan finds is what still blocks.
func (d Deps) releaseAll(ctx context.Context, t detachTarget) ([]releasedRow, []referenceItem, error) {
	scan := referenceScans[t.kind]
	items, err := scan(ctx, d, t.id)
	if err != nil {
		return nil, nil, err
	}
	var released []releasedRow
	for _, it := range callerLast(ctx, items) {
		if !it.detachable {
			continue
		}
		r, err := d.release(ctx, it, t)
		if err != nil {
			return nil, nil, err
		}
		if r != nil {
			released = append(released, *r)
		}
	}
	left, err := scan(ctx, d, t.id)
	if err != nil {
		return nil, nil, err
	}
	return released, left, nil
}

// callerLast moves the caller's own account to the end, so when the
// last-admin guard has to keep one admin, it keeps the caller's.
func callerLast(ctx context.Context, items []referenceItem) []referenceItem {
	a := actor.From(ctx)
	if a == nil || a.UserID == "" {
		return items
	}
	out := make([]referenceItem, 0, len(items))
	var own []referenceItem
	for _, it := range items {
		if it.Kind == "user" && it.ID == a.UserID {
			own = append(own, it)
			continue
		}
		out = append(out, it)
	}
	return append(out, own...)
}

// endSessions ends the sessions of accounts that lost a role, as their own
// update does, once the change has committed.
func (d Deps) endSessions(ctx context.Context, released []releasedRow) error {
	if d.Sessions == nil {
		return nil
	}
	for _, r := range released {
		if r.item.Kind != "user" {
			continue
		}
		if err := d.Sessions.DestroyUser(ctx, r.item.ID); err != nil {
			return huma.Error500InternalServerError(err.Error())
		}
	}
	return nil
}

// recordDetach audits each edited row as its own update, plus the detach of
// the target.
func (d Deps) recordDetach(ctx context.Context, plural string, t detachTarget, released []releasedRow) {
	rows := make([]audit.Row, 0, len(released)+1)
	for _, r := range released {
		rows = append(rows, audit.Row{
			Action: r.action, Fields: r.fields,
			Resource: d.auditResource(r.item.Kind, r.item.ID, r.item.Name, r.item.owner),
		})
	}
	rows = append(rows, audit.Row{Action: plural + ".detach", Resource: d.auditResource(t.kind, t.id, t.name, t.owner)})
	audit.RecordEach(ctx, rows)
}

func (d Deps) auditResource(kind, id, name string, owner meta.Owner) audit.Resource {
	var snap *appcatalog.Snapshot
	if d.Catalog != nil {
		snap = d.Catalog.Current()
	}
	return audit.Resource{Kind: kind, ID: id, Name: name, Owner: &owner, Scope: audit.ScopeOf(snap, &owner)}
}

// targetMeta reads the metadata of a row of one of the referenced kinds; nil
// when no row has id.
func targetMeta(ctx context.Context, s *appcatalog.Stores, kind, id string) (*meta.Metadata, error) {
	switch kind {
	case "provider":
		v, err := s.Provider.Get(ctx, id)
		return rowMeta(v, err, func(v *provider.Provider) *meta.Metadata { return &v.Meta })
	case "host":
		v, err := s.Host.Get(ctx, id)
		return rowMeta(v, err, func(v *host.Host) *meta.Metadata { return &v.Meta })
	case "model":
		v, err := s.Model.Get(ctx, id)
		return rowMeta(v, err, func(v *model.Model) *meta.Metadata { return &v.Meta })
	case "pricing":
		v, err := s.Pricing.Get(ctx, id)
		return rowMeta(v, err, func(v *pricing.Pricing) *meta.Metadata { return &v.Meta })
	case "policy":
		v, err := s.Policy.Get(ctx, id)
		return rowMeta(v, err, func(v *policy.Policy) *meta.Metadata { return &v.Meta })
	case "host-key":
		v, err := s.HostKey.Get(ctx, id)
		return rowMeta(v, err, func(v *hostkey.HostKey) *meta.Metadata { return &v.Meta })
	case "rate-limit":
		v, err := s.RateLimit.Get(ctx, id)
		return rowMeta(v, err, func(v *ratelimit.RateLimit) *meta.Metadata { return &v.Meta })
	case "team":
		v, err := s.Team.Get(ctx, id)
		return rowMeta(v, err, func(v *team.Team) *meta.Metadata { return &v.Meta })
	case "project":
		v, err := s.Project.Get(ctx, id)
		return rowMeta(v, err, func(v *project.Project) *meta.Metadata { return &v.Meta })
	case "service-account":
		v, err := s.ServiceAccount.Get(ctx, id)
		return rowMeta(v, err, func(v *serviceaccount.ServiceAccount) *meta.Metadata { return &v.Meta })
	case "group":
		v, err := s.Group.Get(ctx, id)
		return rowMeta(v, err, func(v *group.Group) *meta.Metadata { return &v.Meta })
	case "role":
		v, err := s.Role.Get(ctx, id)
		return rowMeta(v, err, func(v *role.Role) *meta.Metadata { return &v.Meta })
	}
	return nil, fmt.Errorf("no row lookup for kind %q", kind)
}

func rowMeta[T any](v *T, err error, m func(*T) *meta.Metadata) (*meta.Metadata, error) {
	if err != nil || v == nil {
		return nil, err
	}
	return m(v), nil
}
