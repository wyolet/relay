package plan

import (
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
)

// PoolPlan is one pool: its host, its credentials in pool order, the selection algorithm and the rotation cell. A keyless host's pool holds one anonymous slot; a credential's pool of one has no Pool row and no rotation.
type PoolPlan struct {
	Pool      *pool.Pool
	Host      *host.Host
	Slots     []*KeySlot
	Selection keypool.KeySelection
	Rotation  *keypool.Rotation
	in        inputs
}

func (p *PoolPlan) builtFrom() inputs { return p.in }

// serves reports whether requests can go out through the pool: its host is routed to and one of its slots is not refused.
func (p *PoolPlan) serves() bool {
	return servesRequests(p.Host) && usableSlot(p.Slots)
}

func (c *controllers) poolPlan(tx *engine.Txn, id string) error {
	p := engine.Get[pool.Pool](tx, id)
	if p == nil {
		engine.Drop(tx)
		return nil
	}
	src := engine.Ref{Kind: KindPool, ID: id}
	h := engine.Get[host.Host](tx, p.Spec.HostID)
	if h == nil {
		tx.Issue(parentMissing(src, "spec.hostId", engine.Ref{Kind: KindHost, ID: p.Spec.HostID}))
		engine.Drop(tx)
		return nil
	}
	if !h.IsEnabled() {
		tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: IssueHostDisabled, Source: src, Field: "spec.hostId", Target: engine.Ref{Kind: KindHost, ID: h.Meta.ID},
			Message: "host " + h.Meta.Name + " is disabled; the pool serves nothing"})
	}
	pp := &PoolPlan{Pool: p, Host: h, Selection: p.Spec.Selection}
	if pp.Selection == "" {
		pp.Selection = pool.DefaultSelection
	}
	pp.in = inputs{p, h}
	if h.Spec.NoAuth {
		// A keyless host is reached through a pool linked on purpose, never by a wildcard: the pool is the explicit grant.
		if len(p.Spec.CredentialIDs) > 0 || len(p.Spec.AccountIDs) > 0 {
			field := "spec.credentialIds"
			if len(p.Spec.CredentialIDs) == 0 {
				field = "spec.accountIds"
			}
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueCredentialsIgnored, Source: src, Field: field, Message: "the host needs no key; requests go out without the pool's credentials and accounts"})
		}
		anon := foldUpstream(tx, hostkey.Anonymous(h.Meta.ID, h.Meta.Name), h, nil)
		pp.Slots = []*KeySlot{anon}
		pp.in = append(pp.in, anon.in...)
	} else {
		poolSlots(tx, p, h, pp)
	}
	pp.Rotation = cell.Acquire(c.cells, tx, keypool.RotationKey(id), keypool.NewRotation)
	pp.in = append(pp.in, pp.Rotation)
	engine.Put(tx, SameOr(engine.Get[PoolPlan](tx, id), pp))
	return nil
}

// poolSlots fills pp with the pool's slots in prioritized order: every key of each listed account, in account order and each account's keys in creation order, then the listed credentials; a key reached twice the same way keeps its first place. Two shares reaching one key stay apart, as each lane may use only one of them. A listed member that adds no usable key gets a pool_member_unusable issue.
func poolSlots(tx *engine.Txn, p *pool.Pool, h *host.Host, pp *PoolPlan) {
	src := engine.Ref{Kind: KindPool, ID: p.Meta.ID}
	placed := make(map[[2]string]struct{}, len(p.Spec.CredentialIDs))
	add := func(s *KeySlot) {
		k := [2]string{s.Credential.Meta.ID, s.Via.SubjectID}
		if _, dup := placed[k]; dup {
			return
		}
		placed[k] = struct{}{}
		pp.Slots = append(pp.Slots, s)
	}
	for i, aid := range p.Spec.AccountIDs {
		field := fmt.Sprintf("spec.accountIds[%d]", i)
		a := engine.Get[account.Account](tx, aid)
		switch {
		case a == nil:
			tx.Issue(refMissing(src, field, engine.Ref{Kind: KindAccount, ID: aid}))
			continue
		case a.Spec.HostID != h.Meta.ID:
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueHostMismatch, Source: src, Field: field, Target: engine.Ref{Kind: KindAccount, ID: aid}, Message: "the account is held at another host; it is left out"})
			continue
		}
		// The account's id and host decide its part, so a learned org id or a rename rebuilds no pool.
		pp.in = append(pp.in, aid, a.Spec.HostID)
		var slots []*KeySlot
		shareMissing := false
		if a.IsShared() {
			// A shared account whose real account is gone carries the issue itself.
			sa := engine.Get[SharedAccount](tx, aid)
			pp.in = append(pp.in, sa)
			if sa != nil {
				slots = sa.Slots
			}
			shareMissing = sa == nil
		} else {
			oc := engine.Get[OwnedCredentials](tx, accountSubject(aid))
			pp.in = append(pp.in, oc)
			if oc != nil {
				slots = make([]*KeySlot, 0, len(oc.Pools))
				for _, one := range oc.Pools {
					slots = append(slots, one.Slots[0])
				}
			}
		}
		for _, s := range slots {
			add(s)
		}
		if why := accountUnusable(tx, a, slots, shareMissing); why != "" {
			tx.Issue(memberUnusable(src, field, engine.Ref{Kind: KindAccount, ID: aid}, "account "+a.Meta.Name+" "+why))
		}
	}
	for i, cid := range p.Spec.CredentialIDs {
		field := fmt.Sprintf("spec.credentialIds[%d]", i)
		target := engine.Ref{Kind: KindCredential, ID: cid}
		s := engine.Get[KeySlot](tx, cid)
		if s == nil {
			if k := engine.Get[hostkey.HostKey](tx, cid); k == nil {
				tx.Issue(refMissing(src, field, target))
			} else {
				tx.Issue(memberUnusable(src, field, target, "key "+k.Meta.Name+" "+credentialUnusable(tx, k)))
			}
			continue
		}
		if s.Host.Meta.ID != h.Meta.ID {
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueHostMismatch, Source: src, Field: field, Target: target, Message: "the credential authenticates to another host; it is left out"})
			continue
		}
		pp.in = append(pp.in, s)
		add(s)
		if s.Refused != nil {
			tx.Issue(memberUnusable(src, field, target, "key "+s.Credential.Meta.Name+" serves nothing: "+s.Refused.Message))
		}
	}
}
