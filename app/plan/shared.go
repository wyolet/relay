// A shared account is folded onto its real account at build time: each of the real account's slots is copied with the shared account's rung on top, so the request path reads one slot and makes one limiter call for both. The copy keeps the real account's credential, breaker, cooldown and tier counters, which every holder of the account shares; the shared account's own counters ride in the real account's atomicity group, so the store call stays one.

package plan

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/engine"
)

// SharedAccount is a shared provider account resolved to its real account: the real account's slots, current and future, each narrowed by the shared account's own policy. Absent, with a shared_account_missing issue, while the real account is gone or cannot be shared.
type SharedAccount struct {
	Account *account.Account
	Source  *account.Account
	Slots   []*KeySlot
	in      inputs
}

func (s *SharedAccount) builtFrom() inputs { return s.in }

func (c *controllers) sharedAccount(tx *engine.Txn, id string) error {
	acc := engine.Get[account.Account](tx, id)
	if acc == nil || !acc.IsShared() {
		engine.Drop(tx)
		return nil
	}
	ref := engine.Ref{Kind: KindAccount, ID: id}
	srcID := acc.Spec.SourceAccountID
	src := engine.Get[account.Account](tx, srcID)
	if src == nil || !src.IsProvider() || src.IsShared() || src.Spec.HostID != acc.Spec.HostID {
		tx.Issue(engine.Issue{
			Severity: engine.SeverityError, Code: IssueSharedAccountMissing, Source: ref, Field: "spec.sourceAccountId", Target: engine.Ref{Kind: KindAccount, ID: srcID},
			Message: "the account it was shared from is gone or no longer a provider account at this host; the shared account serves nothing",
		})
		engine.Drop(tx)
		return nil
	}
	if off := personalAccountOff(tx, src.Meta.Owner); off != nil {
		tx.Issue(engine.Issue{
			Severity: engine.SeverityError, Code: IssueSharedAccountMissing, Source: ref, Field: "spec.sourceAccountId", Target: engine.Ref{Kind: KindAccount, ID: srcID},
			Message: "the account it was shared from belongs to " + off.name() + ", whose personal accounts are turned off; the shared account serves nothing until they are on again",
		})
		engine.Drop(tx)
		return nil
	}
	h := engine.Get[host.Host](tx, acc.Spec.HostID)
	if h == nil {
		tx.Issue(parentMissing(ref, "spec.hostId", engine.Ref{Kind: KindHost, ID: acc.Spec.HostID}))
		engine.Drop(tx)
		return nil
	}
	var onlyFor string
	if hd := acc.Spec.Holder; hd != nil {
		onlyFor = hd.Key()
	}
	keys := engine.Get[OwnedCredentials](tx, accountSubject(srcID))
	// The rows' ids, owner, holder and host shape the slots; a rename or a learned org id rebuilds nothing.
	in := inputs{acc.Meta.ID, acc.Meta.Owner, onlyFor, src.Meta.ID, h, keys}
	via := Rung{Kind: limits.RungAccount, SubjectID: accountSubject(id), Account: acc}
	counts, refused := foldAccountRung(tx, h, &via, accountSubject(srcID), &in)
	if prev := engine.Get[SharedAccount](tx, id); prev != nil && prev.in.equal(in) {
		engine.Put(tx, prev)
		return nil
	}
	sa := &SharedAccount{Account: acc, Source: src, in: in}
	if keys != nil {
		sa.Slots = make([]*KeySlot, 0, len(keys.Pools))
		for _, one := range keys.Pools {
			sa.Slots = append(sa.Slots, viaSlot(one.Slots[0], via, counts, refused, onlyFor))
		}
	}
	engine.Put(tx, sa)
	return nil
}

// viaSlot is base reached through the shared account via: base's counters, then the shared account's.
func viaSlot(base *KeySlot, via Rung, counts rungCounts, refused *Refusal, onlyFor string) *KeySlot {
	s := *base
	s.Via, s.OnlyFor, s.in = via, onlyFor, nil
	if s.Refused == nil {
		s.Refused = refused
	}
	s.Whole = append(base.Whole[:len(base.Whole):len(base.Whole)], counts.whole...)
	if len(counts.byModel) > 0 {
		s.ByModel = make(map[string][]slotCounter, len(base.ByModel)+len(counts.byModel))
		for m, cs := range base.ByModel {
			s.ByModel[m] = cs
		}
		for m, cs := range counts.byModel {
			s.ByModel[m] = append(s.ByModel[m][:len(s.ByModel[m]):len(s.ByModel[m])], cs...)
		}
	}
	s.hasBudget = base.hasBudget || counts.hasBudget
	s.Alone = &PoolPlan{Host: base.Host, Slots: []*KeySlot{&s}, Selection: pool.DefaultSelection}
	return &s
}

// forHolder leaves out of pools the slots of a service account's share that a lane of another holder would reach; pools itself when there are none.
func forHolder(pools []*PoolPlan, holder string) []*PoolPlan {
	other := func(s *KeySlot) bool { return s.OnlyFor != "" && s.OnlyFor != holder }
	for i, p := range pools {
		if !containsSlot(p.Slots, other) {
			continue
		}
		out := append(make([]*PoolPlan, 0, len(pools)), pools[:i]...)
		for _, p := range pools[i:] {
			if kept := withoutSlots(p, other); kept != nil {
				out = append(out, kept)
			}
		}
		return out
	}
	return pools
}

func containsSlot(slots []*KeySlot, match func(*KeySlot) bool) bool {
	for _, s := range slots {
		if match(s) {
			return true
		}
	}
	return false
}

// drawsOn is the account a slot's pool lists it by: the shared account it is reached through, else its credential's account.
func (s *KeySlot) drawsOn() string {
	if a := s.Via.Account; a != nil {
		return a.Meta.ID
	}
	return s.Credential.Spec.AccountID
}
