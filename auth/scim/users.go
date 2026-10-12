package scim

import (
	"errors"
	"net/http"
	"slices"

	"github.com/wyolet/relay/auth/directory"
)

func (h *Handler) serveUsers(rq *request, rest []string) error {
	if len(rest) == 0 {
		switch rq.r.Method {
		case http.MethodGet:
			return h.listUsers(rq)
		case http.MethodPost:
			return h.createUser(rq)
		}
		return methodNotAllowed()
	}
	if len(rest) > 1 {
		return errorf(http.StatusNotFound, "", "no such endpoint")
	}
	id := rest[0]
	switch rq.r.Method {
	case http.MethodGet:
		return h.getUser(rq, id)
	case http.MethodPut:
		return h.putUser(rq, id)
	case http.MethodPatch:
		return h.patchUser(rq, id)
	case http.MethodDelete:
		return h.deleteUser(rq, id)
	}
	return methodNotAllowed()
}

// groupsOf returns the client's groups the user is a member of.
func groupsOf(groups []*directory.Group, userID string) []*directory.Group {
	var out []*directory.Group
	for _, g := range groups {
		if slices.Contains(g.Spec.MemberIDs, userID) {
			out = append(out, g)
		}
	}
	return out
}

func (h *Handler) listUsers(rq *request) error {
	lq, err := parseListQuery(rq.r.URL.Query(), userSchema)
	if err != nil {
		return err
	}
	users, err := rq.p.Users(rq.ctx())
	if err != nil {
		return err
	}
	groups, err := rq.p.Groups(rq.ctx())
	if err != nil {
		return err
	}
	var matching []any
	for _, u := range users {
		if lq.filter != nil && !lq.filter.eval(userDoc(u)) {
			continue
		}
		res, err := lq.sel.apply(h.renderUser(u, groupsOf(groups, u.ID)))
		if err != nil {
			return err
		}
		matching = append(matching, res)
	}
	return rq.reply(http.StatusOK, page(matching, lq))
}

// replyUser answers with the stored user, read again so the response shows what was written.
func (h *Handler) replyUser(rq *request, status int, id string) error {
	u, err := rq.p.User(rq.ctx(), id)
	if err != nil {
		return err
	}
	groups, err := rq.p.Groups(rq.ctx())
	if err != nil {
		return err
	}
	sel, err := parseSelection(rq.r.URL.Query(), userSchema)
	if err != nil {
		return err
	}
	res, err := sel.apply(h.renderUser(u, groupsOf(groups, u.ID)))
	if err != nil {
		return err
	}
	if status == http.StatusCreated {
		if loc := h.location("Users", id); loc != "" {
			rq.w.Header().Set("Location", loc)
		}
	}
	return rq.reply(status, res)
}

func (h *Handler) getUser(rq *request, id string) error {
	return h.replyUser(rq, http.StatusOK, id)
}

func (h *Handler) createUser(rq *request) error {
	raw, err := rq.body()
	if err != nil {
		return err
	}
	var in userIn
	if err := decodeBody(raw, &in); err != nil {
		return err
	}
	if in.UserName == nil || *in.UserName == "" {
		return badRequest(typeInvalidValue, "userName is required")
	}
	fields := directory.ProvisionedUser{Username: *in.UserName}
	if in.ExternalID != nil {
		fields.ExternalID = *in.ExternalID
	}
	if in.Emails != nil {
		fields.Email = primaryEmail(*in.Emails)
	}
	disabled := in.Active != nil && !bool(*in.Active)
	u, err := rq.p.CreateUser(rq.ctx(), fields, disabled)
	w := Write{Kind: "user", Op: "create", Name: fields.Username, Err: err}
	if u != nil {
		w.ID = u.ID
	}
	if err := h.report(rq, w); err != nil {
		return err
	}
	return h.replyUser(rq, http.StatusCreated, u.ID)
}

// putUser replaces the attributes the body carries; one it leaves out keeps its stored value, so a client that omits externalId or active does not clear it.
func (h *Handler) putUser(rq *request, id string) error {
	raw, err := rq.body()
	if err != nil {
		return err
	}
	var in userIn
	if err := decodeBody(raw, &in); err != nil {
		return err
	}
	stored, err := h.storedUser(rq, id, "update")
	if err != nil {
		return err
	}
	if in.UserName == nil || *in.UserName == "" {
		return badRequest(typeInvalidValue, "userName is required")
	}
	next := newUserPatch(stored)
	next.userName = *in.UserName
	if in.ExternalID != nil {
		next.externalID = *in.ExternalID
	}
	if in.Emails != nil {
		next.email = primaryEmail(*in.Emails)
	}
	if in.Active != nil {
		next.active = bool(*in.Active)
	}
	if err := h.writeUser(rq, stored, next); err != nil {
		return err
	}
	return h.replyUser(rq, http.StatusOK, id)
}

func (h *Handler) patchUser(rq *request, id string) error {
	raw, err := rq.body()
	if err != nil {
		return err
	}
	ops, err := decodePatch(raw)
	if err != nil {
		return err
	}
	stored, err := h.storedUser(rq, id, "update")
	if err != nil {
		return err
	}
	next := newUserPatch(stored)
	if err := applyPatch(ops, userSchema, next); err != nil {
		return err
	}
	if err := h.writeUser(rq, stored, next); err != nil {
		return err
	}
	return h.replyUser(rq, http.StatusOK, id)
}

// writeUser writes the changed attributes, then the active flag through DisableUser or EnableUser, so a deactivation ends the user's sessions and tokens. A refused deactivation writes nothing.
func (h *Handler) writeUser(rq *request, stored *directory.User, next *userPatch) error {
	if !next.active {
		if err := h.checkDeactivate(rq, stored, "disable"); err != nil {
			return err
		}
	}
	if fields := next.changedFields(stored); len(fields) > 0 {
		_, err := rq.p.UpdateUser(rq.ctx(), stored.ID, next.provisioned())
		if err := h.report(rq, Write{Kind: "user", Op: "update", ID: stored.ID, Name: next.userName, Fields: fields, Err: err}); err != nil {
			return err
		}
	}
	if next.active == !stored.Disabled {
		return nil
	}
	w := Write{Kind: "user", ID: stored.ID, Name: next.userName}
	if next.active {
		w.Op, w.Err = "enable", rq.p.EnableUser(rq.ctx(), stored.ID)
	} else {
		w.Op, w.Err = "disable", rq.p.DisableUser(rq.ctx(), stored.ID)
	}
	return h.report(rq, w)
}

// storedUser reads the user a write targets; a missing or foreign one is reported as a failed op.
func (h *Handler) storedUser(rq *request, id, op string) (*directory.User, error) {
	u, err := rq.p.User(rq.ctx(), id)
	if errors.Is(err, directory.ErrNotFound) {
		return nil, h.report(rq, Write{Kind: "user", Op: op, ID: id, Err: err})
	}
	return u, err
}

func (h *Handler) deleteUser(rq *request, id string) error {
	op := "delete"
	if h.cfg.DeleteDisables {
		op = "disable"
	}
	u, err := h.storedUser(rq, id, op)
	if err != nil {
		return err
	}
	if err := h.checkDeactivate(rq, u, op); err != nil {
		return err
	}
	w := Write{Kind: "user", ID: id, Name: u.Username}
	if h.cfg.DeleteDisables {
		w.Op, w.Err = "disable", rq.p.DisableUser(rq.ctx(), id)
	} else {
		w.Op, w.Err = "delete", rq.p.DeleteUser(rq.ctx(), id)
	}
	if err := h.report(rq, w); err != nil {
		return err
	}
	return rq.noContent()
}
