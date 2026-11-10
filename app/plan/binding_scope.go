// A policy binding governs only accounts inside its owner's scope, and its policy's links reach only pools and credentials that scope may use: the rules app/refcheck enforces on save. The plan holds to them as well, so a row written around the save check (or left behind when an account's owner moved) is ignored or narrowed for that account rather than applied.

package plan

import (
	"fmt"
	"slices"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// bindingsInScope keeps the bindings whose owner's scope holds the account and files an issue for each one it drops.
func bindingsInScope(tx *engine.Txn, acc *account.Account, bs []*policybinding.PolicyBinding) []*policybinding.PolicyBinding {
	kept := bs[:0:0]
	for _, b := range bs {
		if scopeHolds(tx, b.Meta.Owner, acc.Meta.Owner) {
			kept = append(kept, b)
			continue
		}
		tx.Issue(engine.Issue{
			Severity: engine.SeverityError, Code: IssueSubjectOutOfScope, Source: engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, Target: engine.Ref{Kind: KindAccount, ID: acc.Meta.ID}, Field: "spec.accountIds",
			Message: fmt.Sprintf("account %s is owned by %s, outside the scope of the binding's owner (%s); the binding is ignored for it", acc.Meta.Name, ownerKey(acc.Meta.Owner), ownerKey(b.Meta.Owner)),
		})
	}
	return kept
}

// scopeHolds reports whether scope outer holds inner: the system holds everything, a scope itself, a team its projects.
func scopeHolds[R *engine.Txn | *engine.View](r R, outer, inner meta.Owner) bool {
	return outer.Holds(inner, func(projectID string) (string, bool) {
		if p := engine.Get[project.Project](r, projectID); p != nil {
			return p.Spec.TeamID, true
		}
		return "", false
	})
}

// unusableLinks returns the ids of the pools p links, and of the credentials and accounts inside them, that scope cannot use, sorted, and files an issue on the binding for each. The save check refuses such a binding; a row written around it narrows instead of reaching another scope's credentials.
func unusableLinks(tx *engine.Txn, p *policy.Policy, scope meta.Owner, b *policybinding.PolicyBinding) []string {
	return unusableIDs(tx, p, scope, func(kind engine.Kind, id string, owner meta.Owner) {
		tx.Issue(engine.Issue{
			Severity: engine.SeverityError, Code: IssueLinkOutOfScope, Source: engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, Target: engine.Ref{Kind: kind, ID: id}, Field: "spec.poolIds",
			Message: fmt.Sprintf("policy %s links %s %s owned by %s, which %s cannot use; the link is ignored for it", p.Meta.Name, kind, id, ownerKey(owner), ownerKey(scope)),
		})
	})
}

// unusableIDs returns the ids of the pools p links, and of the credentials and accounts inside them, that scope cannot use, sorted, calling refuse for each.
func unusableIDs[R *engine.Txn | *engine.View](r R, p *policy.Policy, scope meta.Owner, refuse func(kind engine.Kind, id string, owner meta.Owner)) []string {
	var out []string
	drop := func(kind engine.Kind, id string, owner meta.Owner) {
		out = append(out, id)
		refuse(kind, id, owner)
	}
	for _, id := range p.Spec.PoolIDs {
		pl := engine.Get[pool.Pool](r, id)
		switch {
		case pl == nil:
		case !scopeHolds(r, pl.Meta.Owner, scope):
			drop(KindPool, id, pl.Meta.Owner)
		default:
			for _, aid := range pl.Spec.AccountIDs {
				if a := engine.Get[account.Account](r, aid); a != nil && !scopeHolds(r, a.Meta.Owner, scope) {
					drop(KindAccount, aid, a.Meta.Owner)
				}
			}
			for _, cid := range pl.Spec.CredentialIDs {
				if k := engine.Get[hostkey.HostKey](r, cid); k != nil && !scopeHolds(r, k.Meta.Owner, scope) {
					drop(KindCredential, cid, k.Meta.Owner)
				}
			}
		}
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
