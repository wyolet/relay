// A policy binding governs only subjects inside its owner's scope, the rule app/refcheck enforces on save. The plan holds to it as well, so a row written around the save check (or left behind when a subject moved) is ignored for that subject rather than applied.

package plan

import (
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/engine"
)

// bindingsInScope keeps the bindings whose owner's scope holds subject and files an issue for each one it drops. Users are left to splitScoped: a team- or project-owned binding on a user governs only the user's requests inside that scope.
func bindingsInScope(tx *engine.Txn, subject string, bs []*policybinding.PolicyBinding) []*policybinding.PolicyBinding {
	if strings.HasPrefix(subject, "user:") {
		return bs
	}
	in, ok := subjectScope(tx, subject)
	if !ok {
		return bs
	}
	kept := bs[:0:0]
	for _, b := range bs {
		if scopeHolds(tx, b.Meta.Owner, in) {
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

func ownerKey(o meta.Owner) string {
	if o.ID == "" {
		return string(o.Kind)
	}
	return string(o.Kind) + ":" + o.ID
}
