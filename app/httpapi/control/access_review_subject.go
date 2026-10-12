package control

import (
	"context"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/rolebinding"
)

// namesCaller reports whether in names the signed-in user making the request.
func namesCaller(caller *actor.Actor, in reviewSubjectInput) bool {
	if in.Kind != string(rolebinding.SubjectUser) || caller == nil || caller.UserID == "" {
		return false
	}
	if in.ID != "" {
		return in.ID == caller.UserID
	}
	return in.Name != "" && in.Name == caller.Username
}

// callerShown describes the caller as authentication built it; their subjects include the groups their identity provider asserted at sign-in.
func (r accessReviewer) callerShown(ctx context.Context, caller *actor.Actor) reviewedSubject {
	if caller.UserID == "" {
		return reviewedSubject{Kind: "admin-token", Name: caller.Username, Admin: authz.IsAdmin(ctx)}
	}
	return reviewedSubject{Kind: string(rolebinding.SubjectUser), ID: caller.UserID, Name: caller.Username,
		Admin: authz.IsAdmin(ctx), Subjects: caller.Subjects}
}

// subject builds the principal in names as authentication would. A user carries their roles and local groups; the groups an identity provider asserts exist only on a live session, so they are not part of the answer.
func (r accessReviewer) subject(ctx context.Context, in reviewSubjectInput) (*actor.Actor, reviewedSubject, error) {
	ref := in.ID
	if ref == "" {
		ref = in.Name
	}
	if ref == "" {
		return nil, reviewedSubject{}, huma.Error400BadRequest("subject needs a name or an id")
	}
	notFound := huma.Error404NotFound(fmt.Sprintf("%s %q not found", in.Kind, ref))
	snap := r.snapshot()
	var a *actor.Actor
	shown := reviewedSubject{Kind: in.Kind}
	switch rolebinding.SubjectKind(in.Kind) {
	case rolebinding.SubjectUser:
		if r.users == nil {
			return nil, reviewedSubject{}, notFound
		}
		u, err := findUser(ctx, r.users, ref)
		if err != nil {
			return nil, reviewedSubject{}, notFound
		}
		var groups []string
		if snap != nil {
			groups = snap.GroupsForUser(u.ID)
		}
		a = &actor.Actor{UserID: u.ID, Username: u.Username, Roles: u.Roles, Subjects: appcatalog.UserSubjects(u.ID, groups, nil)}
		shown.Disabled = u.Disabled
	case rolebinding.SubjectServiceAccount:
		kr, ok := r.rows["service-accounts"]
		if !ok || snap == nil {
			return nil, reviewedSubject{}, notFound
		}
		res, err := kr.find(ctx, ref)
		if err != nil {
			return nil, reviewedSubject{}, notFound
		}
		_, enabled := snap.ServiceAccountByName(res.Name)
		a = &actor.Actor{UserID: res.ID, Username: res.Name, Subjects: snap.SubjectsForServiceAccount(res.ID)}
		shown.Disabled = !enabled
	default:
		return nil, reviewedSubject{}, huma.Error400BadRequest("subject kind must be user or serviceaccount")
	}
	shown.ID, shown.Name, shown.Subjects = a.UserID, a.Username, a.Subjects
	shown.Admin = authz.IsAdmin(actor.WithActor(ctx, a))
	return a, shown, nil
}
