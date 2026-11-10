// Why a pool a policy links, or an account or key in it, brings a lane nothing: so a reach read that comes back empty always says why.

package plan

import (
	"slices"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/meta"
)

// Codes of a dropped link, besides the issue codes ref_missing, link_out_of_scope, host_disabled, host_mismatch and pool_member_unusable.
const (
	// DroppedHeldForAnotherLane is a service account's share, which serves only that service account's lane.
	DroppedHeldForAnotherLane = "held_for_another_lane"
	// DroppedNarrowedByRung is a link another rung's pools leave out.
	DroppedNarrowedByRung = "narrowed_by_rung"
)

// DroppedLink is a pool the policy links, or an account or key the pool lists, that brings the lane nothing. Pool is nil, and PoolID set, when the pool row is gone.
type DroppedLink struct {
	PoolID     string
	Pool       *pool.Pool
	Account    *account.Account
	Credential *hostkey.HostKey
	Code       string
	Reason     string
}

// droppedLinks lists what the policy ep links that adds nothing to cp's pools, read for an account in scope; unusable are the links scope may not use, sorted.
func droppedLinks[R *engine.Txn | *engine.View](r R, cp *CallerPlan, ep *ExpandedPolicy, scope meta.Owner, unusable []string) []DroppedLink {
	if cp.Refused != nil || !ep.links() {
		return nil
	}
	final := map[*KeySlot]bool{}
	for _, p := range cp.Pools {
		for _, s := range p.Slots {
			final[s] = true
		}
	}
	reached := func(slots []*KeySlot) bool {
		return slices.ContainsFunc(slots, func(s *KeySlot) bool { return final[s] })
	}
	outOfScope := func(id string) bool { _, ok := slices.BinarySearch(unusable, id); return ok }
	var out []DroppedLink
	for _, pid := range ep.Policy.Spec.PoolIDs {
		pl := engine.Get[pool.Pool](r, pid)
		if pl == nil {
			out = append(out, DroppedLink{PoolID: pid, Code: IssueRefMissing, Reason: "links a pool that was deleted"})
			continue
		}
		d := DroppedLink{PoolID: pid, Pool: pl}
		if outOfScope(pid) {
			out = append(out, d.with(IssueLinkOutOfScope, "pool "+pl.Meta.Name+" belongs to "+scopeName(r, pl.Meta.Owner)+", which "+scopeName(r, scope)+" cannot use"))
			continue
		}
		pp := engine.Get[PoolPlan](r, pid)
		switch {
		case pp == nil:
			out = append(out, d.with(IssueRefMissing, "pool "+pl.Meta.Name+" names a host that was deleted"))
			continue
		case !servesRequests(pp.Host):
			out = append(out, d.with(IssueHostDisabled, "host "+pp.Host.Meta.Name+" of pool "+pl.Meta.Name+" is disabled"))
			continue
		case pp.Host.Spec.NoAuth:
			if !reached(pp.Slots) {
				out = append(out, leftOut(r, cp, d, ep, pp.Slots))
			}
			continue
		}
		for _, aid := range pl.Spec.AccountIDs {
			a := engine.Get[account.Account](r, aid)
			m := d
			m.Account = a
			switch {
			case a == nil:
				out = append(out, m.with(IssueRefMissing, "pool "+pl.Meta.Name+" lists an account that was deleted"))
			case outOfScope(aid):
				out = append(out, m.with(IssueLinkOutOfScope, "account "+a.Meta.Name+" belongs to "+scopeName(r, a.Meta.Owner)+", which "+scopeName(r, scope)+" cannot use"))
			case a.Spec.HostID != pp.Host.Meta.ID:
				out = append(out, m.with(IssueHostMismatch, "account "+a.Meta.Name+" is held at another host than pool "+pl.Meta.Name))
			default:
				slots := slotsWhere(pp.Slots, func(s *KeySlot) bool { return s.drawsOn() == aid })
				shareMissing := a.IsShared() && engine.Get[SharedAccount](r, aid) == nil
				if why := accountUnusable(r, a, slots, shareMissing); why != "" {
					out = append(out, m.with(IssuePoolMemberUnusable, "account "+a.Meta.Name+" "+why))
				} else if !reached(slots) {
					out = append(out, leftOut(r, cp, m, ep, slots))
				}
			}
		}
		for _, cid := range pl.Spec.CredentialIDs {
			k := engine.Get[hostkey.HostKey](r, cid)
			m := d
			m.Credential = k
			slots := slotsWhere(pp.Slots, func(s *KeySlot) bool { return s.Credential.Meta.ID == cid && s.Via.SubjectID == "" })
			switch {
			case k == nil:
				out = append(out, m.with(IssueRefMissing, "pool "+pl.Meta.Name+" lists a host key that was deleted"))
			case outOfScope(cid):
				out = append(out, m.with(IssueLinkOutOfScope, "host key "+k.Meta.Name+" belongs to "+scopeName(r, k.Meta.Owner)+", which "+scopeName(r, scope)+" cannot use"))
			case k.Spec.HostID != pp.Host.Meta.ID:
				out = append(out, m.with(IssueHostMismatch, "host key "+k.Meta.Name+" authenticates to another host than pool "+pl.Meta.Name))
			case len(slots) == 0:
				out = append(out, m.with(IssuePoolMemberUnusable, "host key "+k.Meta.Name+" "+credentialUnusable(r, k)))
			case !usableSlot(slots):
				out = append(out, m.with(IssuePoolMemberUnusable, "host key "+k.Meta.Name+" serves nothing: "+slots[0].Refused.Message))
			case !reached(slots):
				out = append(out, leftOut(r, cp, m, ep, slots))
			}
		}
	}
	return out
}

func (d DroppedLink) with(code, reason string) DroppedLink {
	d.Code, d.Reason = code, reason
	return d
}

func slotsWhere(slots []*KeySlot, keep func(*KeySlot) bool) []*KeySlot {
	var out []*KeySlot
	for _, s := range slots {
		if keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// leftOut says why slots, usable in their pool, are not among cp's: a service account's share held for another lane, or another rung's pools leaving them out.
func leftOut[R *engine.Txn | *engine.View](r R, cp *CallerPlan, d DroppedLink, ep *ExpandedPolicy, slots []*KeySlot) DroppedLink {
	if len(slots) > 0 && !slices.ContainsFunc(slots, func(s *KeySlot) bool { return s.OnlyFor == "" || s.OnlyFor == cp.Holder }) {
		s := slots[0]
		_, saID, _ := strings.Cut(s.OnlyFor, ":")
		what, who := "shared account "+s.Via.Account.Meta.Name, s.OnlyFor
		if sa := engine.Get[serviceaccount.ServiceAccount](r, saID); sa != nil {
			who = "service account " + sa.Meta.Name
		}
		reason := what + " is held for " + who + "; it serves only that account's lane"
		if m := engine.Get[ManagedAccount](r, "service:"+saID); m != nil && m.Row != nil {
			reason += ": pass account=" + m.Row.Meta.ID
		}
		return d.with(DroppedHeldForAnotherLane, reason)
	}
	if i := cp.linkedAt; i >= 0 && cp.Rungs[i].Policy != ep {
		rg := &cp.Rungs[i]
		return d.with(DroppedNarrowedByRung, rungPolicyName(r, rg)+" decides the pools this lane reaches and does not link its keys")
	}
	for i := cp.linkedAt - 1; i >= 0; i-- {
		if rg := &cp.Rungs[i]; rg.Policy.links() {
			return d.with(DroppedNarrowedByRung, rungPolicyName(r, rg)+" links pools that do not hold its keys, so the lane leaves them out")
		}
	}
	return d.with(DroppedNarrowedByRung, "the lane leaves its keys out")
}

// rungPolicyName names a caller rung's policy for a reader, as "team platform's policy platform-default".
func rungPolicyName[R *engine.Txn | *engine.View](r R, rg *Rung) string {
	holder := "the " + string(rg.Kind) + " rung"
	if rg.Account != nil {
		holder = rungHolder(r, rg.Kind, rg.Account)
	}
	return holder + "'s policy " + rg.Policy.Policy.Meta.Name
}

// scopeName names an owner scope for a reader.
func scopeName[R *engine.Txn | *engine.View](r R, o meta.Owner) string {
	switch o.Kind {
	case meta.OwnerSystem:
		return "the system"
	case meta.OwnerTeam:
		if t := engine.Get[team.Team](r, o.ID); t != nil {
			return "team " + t.Meta.Name
		}
	case meta.OwnerProject:
		if p := engine.Get[project.Project](r, o.ID); p != nil {
			return "project " + p.Meta.Name
		}
	case meta.OwnerUser:
		if u := engine.Get[user.User](r, o.ID); u != nil {
			return "user " + u.Username
		}
	case meta.OwnerHost:
		if h := engine.Get[host.Host](r, o.ID); h != nil {
			return "host " + h.Meta.Name
		}
	}
	return ownerKey(o)
}
