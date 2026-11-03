// Why a pool member adds no usable key. The member itself carries the detailed issue (its own account, credential or share); the pool repeats the reason in plain words so the pool and the policies linking it show why they serve less.

package plan

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/pkg/engine"
)

func memberUnusable(src engine.Ref, field string, target engine.Ref, what string) engine.Issue {
	return engine.Issue{Severity: engine.SeverityWarning, Code: IssuePoolMemberUnusable, Source: src, Target: target, Field: field, Message: what + "; the pool skips it"}
}

// usableSlot reports whether one of slots is not refused.
func usableSlot(slots []*KeySlot) bool {
	for _, s := range slots {
		if s.Refused == nil {
			return true
		}
	}
	return false
}

// accountUnusable says why account a, listed in a pool, adds no usable key, given the slots it added; "" when one of them is usable. shareMissing is a shared account with no resolved real account.
func accountUnusable(tx *engine.Txn, a *account.Account, slots []*KeySlot, shareMissing bool) string {
	if usableSlot(slots) {
		return ""
	}
	switch {
	case shareMissing:
		if src := engine.Get[account.Account](tx, a.Spec.SourceAccountID); src != nil {
			if off := personalAccountOff(tx, src.Meta.Owner); off != nil {
				return "is shared from an account of " + off.name() + ", whose personal accounts are turned off"
			}
		}
		return "is shared from an account that is gone or no longer a provider account at this host"
	case !a.IsShared():
		if off := personalAccountOff(tx, a.Meta.Owner); off != nil {
			return "belongs to " + off.name() + ", whose personal accounts are turned off"
		}
	}
	for _, s := range slots {
		if s.Refused != nil {
			return "serves nothing: " + s.Refused.Message
		}
	}
	return "has no usable key"
}

// credentialUnusable says why credential k, listed in a pool, has no slot.
func credentialUnusable(tx *engine.Txn, k *hostkey.HostKey) string {
	switch {
	case !k.IsEnabled():
		return "is disabled"
	case k.Status.Unresolved != nil || k.KeyHash == "":
		return "has no value relay can resolve"
	}
	if aid := k.Spec.AccountID; aid != "" {
		if a := engine.Get[account.Account](tx, aid); a != nil {
			if off := personalAccountOff(tx, a.Meta.Owner); off != nil {
				return "is in account " + a.Meta.Name + " of " + off.name() + ", whose personal accounts are turned off"
			}
		}
	}
	return "cannot serve; see the key's own issues"
}
