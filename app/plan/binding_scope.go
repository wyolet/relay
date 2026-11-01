// A policy binding governs only subjects inside its owner's scope, and its policy's links reach only pools and credentials that scope may use: the rules app/refcheck enforces on save. The plan holds to them as well, so a row written around the save check (or left behind when a subject moved) is ignored or narrowed for that subject rather than applied.

package plan

import (
	"fmt"
	"slices"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/engine"
)

// bindingsInScope keeps the bindings whose owner's scope holds subject and files an issue for each one it drops. A user is held by the user's own scope only; the other scopes are left to splitScoped: a team- or project-owned binding on a user governs only the user's requests inside that scope.
func bindingsInScope(tx *engine.Txn, subject string, bs []*policybinding.PolicyBinding) []*policybinding.PolicyBinding {
	in, ok := subjectScope(tx, subject)
	if !ok {
		return bs
	}
	kept := bs[:0:0]
	for _, b := range bs {
		if scopeHolds(tx, b.Meta.Owner, in) || in.Kind == meta.OwnerUser && b.Meta.Owner.Kind != meta.OwnerUser {
			kept = append(kept, b)
			continue
		}
		tx.Issue(engine.Issue{
			Severity: engine.SeverityError, Code: IssueSubjectOutOfScope, Source: engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, Field: "spec.subjects",
			Message: fmt.Sprintf("%s is outside the scope of the binding's owner (%s); the binding is ignored for it", subject, ownerKey(b.Meta.Owner)),
		})
	}
	return kept
}

// subjectScope is the scope a subject lives in; a host lives above every scope but the system's. ok is false when the subject's row is gone, which leaves the bindings as they are: a missing subject has no plan to govern.
func subjectScope(tx *engine.Txn, subject string) (meta.Owner, bool) {
	kind, id, _ := strings.Cut(subject, ":")
	switch kind {
	case "user":
		return meta.Owner{Kind: meta.OwnerUser, ID: id}, true
	case "team":
		return meta.Owner{Kind: meta.OwnerTeam, ID: id}, true
	case "project":
		return meta.Owner{Kind: meta.OwnerProject, ID: id}, true
	case "serviceaccount":
		if sa := engine.Get[serviceaccount.ServiceAccount](tx, id); sa != nil {
			return meta.Owner{Kind: meta.OwnerProject, ID: sa.Spec.ProjectID}, true
		}
	case "account":
		if a := engine.Get[account.Account](tx, id); a != nil {
			return a.Meta.Owner, true
		}
	case "credential":
		if k := engine.Get[hostkey.HostKey](tx, id); k != nil {
			return k.Meta.Owner, true
		}
	case "host":
		return meta.Owner{Kind: meta.OwnerHost, ID: id}, true
	}
	return meta.Owner{}, false
}

// scopeHolds reports whether scope outer holds inner: the system holds everything, a scope itself, a team its projects.
func scopeHolds(tx *engine.Txn, outer, inner meta.Owner) bool {
	switch {
	case outer.Kind == meta.OwnerSystem, outer == inner:
		return true
	case outer.Kind == meta.OwnerTeam && inner.Kind == meta.OwnerProject:
		p := engine.Get[project.Project](tx, inner.ID)
		return p != nil && p.Spec.TeamID == outer.ID
	}
	return false
}

// linkScope is the scope whose pools and credentials a caller subject may use through its policy's links: the subject's own, or for a user the scope of the binding governing it (the user's own under a system binding). ok is false for upstream subjects, whose rungs follow no links, and for a missing subject.
func linkScope(tx *engine.Txn, subject string, b *policybinding.PolicyBinding) (meta.Owner, bool) {
	kind, id, _ := strings.Cut(subject, ":")
	switch kind {
	case "user":
		if b.Meta.Owner.Kind == meta.OwnerSystem {
			return meta.Owner{Kind: meta.OwnerUser, ID: id}, true
		}
		return b.Meta.Owner, true
	case "team", "project", "serviceaccount":
		return subjectScope(tx, subject)
	}
	return meta.Owner{}, false
}

// unusableLinks returns the ids of the pools and credentials p links, directly or inside a linked pool, that scope cannot use, sorted, and files an issue on the binding for each. The save check refuses such a binding; a row written around it narrows instead of reaching another scope's credentials.
func unusableLinks(tx *engine.Txn, p *policy.Policy, scope meta.Owner, b *policybinding.PolicyBinding) []string {
	var out []string
	refuse := func(kind engine.Kind, id string, owner meta.Owner, field string) {
		out = append(out, id)
		tx.Issue(engine.Issue{
			Severity: engine.SeverityError, Code: IssueLinkOutOfScope, Source: engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, Target: engine.Ref{Kind: kind, ID: id}, Field: field,
			Message: fmt.Sprintf("policy %s links %s %s owned by %s, which %s cannot use; the link is ignored for it", p.Meta.Name, kind, id, ownerKey(owner), ownerKey(scope)),
		})
	}
	credential := func(id, field string) {
		if k := engine.Get[hostkey.HostKey](tx, id); k != nil && !scopeHolds(tx, k.Meta.Owner, scope) {
			refuse(KindCredential, id, k.Meta.Owner, field)
		}
	}
	for _, id := range p.Spec.PoolIDs {
		pl := engine.Get[pool.Pool](tx, id)
		switch {
		case pl == nil:
		case !scopeHolds(tx, pl.Meta.Owner, scope):
			refuse(KindPool, id, pl.Meta.Owner, "spec.poolIds")
		default:
			for _, cid := range pl.Spec.CredentialIDs {
				credential(cid, "spec.poolIds")
			}
		}
	}
	for _, id := range p.Spec.CredentialIDs {
		credential(id, "spec.credentialIds")
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func ownerKey(o meta.Owner) string {
	if o.ID == "" {
		return string(o.Kind)
	}
	return string(o.Kind) + ":" + o.ID
}
