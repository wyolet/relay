package control

import (
	"context"
	"strings"

	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/auth/rbac"
)

// render is the explanation with every id the reader would otherwise have to look up named.
func (r accessReviewer) render(ctx context.Context, x rbac.Explanation, subject reviewedSubject, action string, res reviewedResource) accessReview {
	snap := r.snapshot()
	out := accessReview{
		Allowed: x.Allowed, Reason: string(x.Reason), Rule: x.Rule, CredentialDenied: x.CredentialDenied,
		Subject: subject, Action: action, Resource: res,
	}
	for _, s := range x.Chain {
		out.Chain = append(out.Chain, r.scopeRef(ctx, snap, ownerOfScope(s)))
	}
	if x.RoleID != "" {
		out.Binding = r.binding(ctx, snap, x)
	}
	return out
}

func (r accessReviewer) binding(ctx context.Context, snap *appcatalog.Snapshot, x rbac.Explanation) *reviewedBinding {
	b := &reviewedBinding{
		Subject: r.subjectRef(ctx, snap, x.Subject),
		Role:    namedRef{Kind: "role", ID: x.RoleID},
		Scope:   r.scopeRef(ctx, snap, ownerOfScope(x.Scope)),
	}
	if snap == nil {
		return b
	}
	if rb, ok := authz.MatchedBinding(snap, x); ok {
		b.ID, b.Name = rb.Meta.ID, rb.Meta.Name
	}
	if role, ok := snap.Role(x.RoleID); ok {
		b.Role.Name = role.Meta.Name
	}
	return b
}

func ownerOfScope(s rbac.Scope) meta.Owner {
	return meta.Owner{Kind: meta.OwnerKind(s.Kind), ID: s.ID}
}

func (r accessReviewer) ownerRef(ctx context.Context, o *meta.Owner) *namedRef {
	if o == nil {
		return nil
	}
	ref := r.scopeRef(ctx, r.snapshot(), *o)
	return &ref
}

func (r accessReviewer) scopeRef(ctx context.Context, snap *appcatalog.Snapshot, o meta.Owner) namedRef {
	ref := namedRef{Kind: string(o.Kind), ID: o.ID}
	if o.Kind == meta.OwnerUser {
		ref.Name = r.username(ctx, o.ID)
		return ref
	}
	if snap == nil || o.ID == "" {
		return ref
	}
	switch o.Kind {
	case meta.OwnerTeam:
		if t, ok := snap.Team(o.ID); ok {
			ref.Name = t.Meta.Name
		}
	case meta.OwnerProject:
		if p, ok := snap.Project(o.ID); ok {
			ref.Name = p.Meta.Name
		}
	case meta.OwnerHost:
		ref.Name, _ = snap.HostSlug(o.ID)
	case meta.OwnerProvider:
		ref.Name, _ = snap.ProviderSlug(o.ID)
	}
	return ref
}

// subjectRef splits a binding subject ("user:<id>", "group:<name>", "serviceaccount:<id>") into its kind and a name.
func (r accessReviewer) subjectRef(ctx context.Context, snap *appcatalog.Snapshot, subject string) namedRef {
	kind, rest, _ := strings.Cut(subject, ":")
	switch kind {
	case "user":
		return namedRef{Kind: kind, ID: rest, Name: r.username(ctx, rest)}
	case "serviceaccount":
		ref := namedRef{Kind: kind, ID: rest}
		if snap != nil {
			if sa, ok := snap.ServiceAccount(rest); ok {
				ref.Name = sa.Meta.Name
			}
		}
		return ref
	}
	return namedRef{Kind: kind, Name: rest}
}

func (r accessReviewer) username(ctx context.Context, id string) string {
	if r.users == nil || id == "" {
		return ""
	}
	u, err := r.users.Get(ctx, id)
	if err != nil || u == nil {
		return ""
	}
	return u.Username
}
