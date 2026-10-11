package scim

import (
	"errors"
	"net/http"

	"github.com/wyolet/relay/auth/directory"
)

// groupWriteAttempts bounds the retries of a group write that lost a race with another change to the same group, as when an identity provider sends membership changes in parallel.
const groupWriteAttempts = 5

func (h *Handler) serveGroups(rq *request, rest []string) error {
	if len(rest) == 0 {
		switch rq.r.Method {
		case http.MethodGet:
			return h.listGroups(rq)
		case http.MethodPost:
			return h.createGroup(rq)
		}
		return methodNotAllowed()
	}
	if len(rest) > 1 {
		return errorf(http.StatusNotFound, "", "no such endpoint")
	}
	id := rest[0]
	switch rq.r.Method {
	case http.MethodGet:
		return h.replyGroup(rq, http.StatusOK, id)
	case http.MethodPut:
		return h.putGroup(rq, id)
	case http.MethodPatch:
		return h.patchGroup(rq, id)
	case http.MethodDelete:
		return h.deleteGroup(rq, id)
	}
	return methodNotAllowed()
}

func (h *Handler) listGroups(rq *request) error {
	lq, err := parseListQuery(rq.r.URL.Query(), groupSchema)
	if err != nil {
		return err
	}
	groups, err := rq.p.Groups(rq.ctx())
	if err != nil {
		return err
	}
	var matching []any
	for _, g := range groups {
		if lq.filter != nil && !lq.filter.eval(groupDoc(g)) {
			continue
		}
		res, err := lq.sel.apply(h.renderGroup(g))
		if err != nil {
			return err
		}
		matching = append(matching, res)
	}
	return rq.reply(http.StatusOK, page(matching, lq))
}

func (h *Handler) replyGroup(rq *request, status int, id string) error {
	g, err := rq.p.Group(rq.ctx(), id)
	if err != nil {
		return err
	}
	sel, err := parseSelection(rq.r.URL.Query(), groupSchema)
	if err != nil {
		return err
	}
	res, err := sel.apply(h.renderGroup(g))
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		if loc := h.location("Groups", id); loc != "" {
			rq.w.Header().Set("Location", loc)
		}
	}
	return rq.reply(status, res)
}

func (h *Handler) createGroup(rq *request) error {
	raw, err := rq.body()
	if err != nil {
		return err
	}
	var in groupIn
	if err := decodeBody(raw, &in); err != nil {
		return err
	}
	if in.DisplayName == nil || *in.DisplayName == "" {
		return badRequest(typeInvalidValue, "displayName is required")
	}
	fields := directory.ProvisionedGroup{DisplayName: *in.DisplayName}
	if in.ExternalID != nil {
		fields.ExternalID = *in.ExternalID
	}
	if in.Members != nil {
		if fields.MemberIDs, err = memberIDs(*in.Members); err != nil {
			return err
		}
	}
	g, err := rq.p.CreateGroup(rq.ctx(), fields)
	w := Write{Kind: "group", Op: "create", Name: fields.DisplayName, Err: err}
	if g != nil {
		w.ID = g.Meta.ID
	}
	if err := h.report(rq, w); err != nil {
		return err
	}
	return h.replyGroup(rq, http.StatusCreated, g.Meta.ID)
}

// putGroup replaces the attributes the body carries; one it leaves out keeps its stored value, so a rename that omits members does not empty the group.
func (h *Handler) putGroup(rq *request, id string) error {
	raw, err := rq.body()
	if err != nil {
		return err
	}
	var in groupIn
	if err := decodeBody(raw, &in); err != nil {
		return err
	}
	if in.DisplayName == nil || *in.DisplayName == "" {
		return badRequest(typeInvalidValue, "displayName is required")
	}
	var members []string
	if in.Members != nil {
		if members, err = memberIDs(*in.Members); err != nil {
			return err
		}
	}
	err = h.updateGroup(rq, id, func(next *groupPatch) error {
		next.displayName = *in.DisplayName
		if in.ExternalID != nil {
			next.externalID = *in.ExternalID
		}
		if in.Members != nil {
			next.setMembers(members)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return h.replyGroup(rq, http.StatusOK, id)
}

// patchGroup answers 204, as both major identity providers accept, so a large group's members are not sent back on every change.
func (h *Handler) patchGroup(rq *request, id string) error {
	raw, err := rq.body()
	if err != nil {
		return err
	}
	ops, err := decodePatch(raw)
	if err != nil {
		return err
	}
	if err := h.updateGroup(rq, id, func(next *groupPatch) error { return applyPatch(ops, groupSchema, next) }); err != nil {
		return err
	}
	return rq.noContent()
}

// updateGroup applies edit to the stored group and writes the result, starting over from a fresh read when another write got in between.
func (h *Handler) updateGroup(rq *request, id string, edit func(*groupPatch) error) error {
	for attempt := 1; ; attempt++ {
		g, err := rq.p.Group(rq.ctx(), id)
		if err != nil {
			if errors.Is(err, directory.ErrNotFound) {
				return h.report(rq, Write{Kind: "group", Op: "update", ID: id, Err: err})
			}
			return err
		}
		next := newGroupPatch(g)
		if err := edit(next); err != nil {
			return err
		}
		fields := next.changedFields(g)
		if len(fields) == 0 {
			return nil
		}
		_, err = rq.p.UpdateGroup(rq.ctx(), id, next.provisioned(), g.Meta.ResourceVersion)
		if errors.Is(err, directory.ErrStaleResourceVersion) && attempt < groupWriteAttempts {
			continue
		}
		return h.report(rq, Write{Kind: "group", Op: "update", ID: id, Name: next.displayName, Fields: fields, Err: err})
	}
}

func (h *Handler) deleteGroup(rq *request, id string) error {
	w := Write{Kind: "group", Op: "delete", ID: id}
	g, err := rq.p.Group(rq.ctx(), id)
	switch {
	case err == nil:
		w.Name = g.Meta.DisplayName
		w.Err = rq.p.DeleteGroup(rq.ctx(), id)
	case errors.Is(err, directory.ErrNotFound):
		w.Err = err
	default:
		return err
	}
	if err := h.report(rq, w); err != nil {
		return err
	}
	return rq.noContent()
}
