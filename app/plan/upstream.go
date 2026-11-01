package plan

import (
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/engine/cell"
)

// Rung is one level of the ladder as a plan holds it: the subject and the policy bound to it. A nil Policy does not constrain.
type Rung struct {
	Kind      limits.Rung
	SubjectID string
	Policy    *ExpandedPolicy
}

// AccountPlan is one provider account and its cooldown cell, shared by every credential of the account.
type AccountPlan struct {
	Account  *account.Account
	Cooldown *keypool.CooldownCell
	in       inputs
}

func (a *AccountPlan) builtFrom() inputs { return a.in }

func (c *controllers) accountPlan(tx *engine.Txn, id string) error {
	acc := engine.Get[account.Account](tx, id)
	if acc == nil {
		engine.Drop(tx)
		return nil
	}
	cd := cell.Acquire(c.cells, tx, keypool.CooldownCellKey(id), keypool.NewCooldownCell)
	// Built from the id and host alone, so a learned org id written onto the row keeps this plan and everything that reads it.
	ap := &AccountPlan{Account: acc, Cooldown: cd, in: inputs{acc.Meta.ID, acc.Spec.HostID, cd}}
	engine.Put(tx, SameOr(engine.Get[AccountPlan](tx, id), ap))
	return nil
}

// KeySlot is one credential with the upstream half of the ladder folded: its host and account, the policies bound to them and to the credential, the counters those impose, and the breaker cell. One KeySlot is shared by every pool holding the credential.
type KeySlot struct {
	Credential *hostkey.HostKey
	Host       *host.Host
	Account    *AccountPlan         // nil when the credential names no account
	Breaker    *keypool.BreakerCell // nil for a keyless host's anonymous slot
	Upstream   [3]Rung              // host, account, credential
	// Refused is set when an upstream rung's policy is disabled or its binding ambiguous: the credential then serves nothing, rather than serving unmetered.
	Refused *Refusal
	// Whole holds the upstream rules with no model scope and the upstream budgets, outer rung first; ByModel the scoped rules, expanded per model id.
	Whole   []limits.Counter
	ByModel map[string][]limits.Counter
	// Alone is the credential as a pool of one, for policies that link it directly and for reach by ownership.
	Alone     *PoolPlan
	hasBudget bool
	budgetAt  int // index into Upstream of the first rung with a budget
	in        inputs
}

func (s *KeySlot) builtFrom() inputs { return s.in }

// rejects returns the index of the first upstream rung whose grant does not cover the model at this slot's host, or -1.
func (s *KeySlot) rejects(providerSlug string, m *model.Model, hostSlug string) int {
	for i := range s.Upstream {
		if p := s.Upstream[i].Policy; p != nil && p.Grant != nil && !p.Grant.Allows(providerSlug, m, hostSlug) {
			return i
		}
	}
	return -1
}

func (c *controllers) keySlot(tx *engine.Txn, id string) error {
	cred := engine.Get[hostkey.HostKey](tx, id)
	if cred == nil || !cred.IsEnabled() {
		engine.Drop(tx)
		return nil
	}
	src := engine.Ref{Kind: KindCredential, ID: id}
	if cred.Status.Unresolved != nil || cred.KeyHash == "" {
		reason := "no value"
		if cred.Status.Unresolved != nil {
			reason = cred.Status.Unresolved.Reason
		}
		tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: IssueCredentialUnresolved, Source: src, Field: "spec.valueFrom", Message: "credential value could not be resolved: " + reason})
		engine.Drop(tx)
		return nil
	}
	h := engine.Get[host.Host](tx, cred.Spec.HostID)
	if h == nil {
		tx.Issue(parentMissing(src, "spec.hostId", engine.Ref{Kind: KindHost, ID: cred.Spec.HostID}))
		engine.Drop(tx)
		return nil
	}
	var acc *AccountPlan
	if aid := cred.Spec.AccountID; aid != "" {
		if acc = engine.Get[AccountPlan](tx, aid); acc == nil {
			tx.Issue(parentMissing(src, "spec.accountId", engine.Ref{Kind: KindAccount, ID: aid}))
			engine.Drop(tx)
			return nil
		}
		if acc.Account.Spec.HostID != h.Meta.ID {
			tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: IssueHostMismatch, Source: src, Field: "spec.accountId", Target: engine.Ref{Kind: KindAccount, ID: aid}, Message: "the account is held at another host"})
			engine.Drop(tx)
			return nil
		}
	}
	s := foldUpstream(tx, cred, h, acc)
	s.Breaker = cell.Acquire(c.cells, tx, keypool.BreakerCellKey(cred.KeyHash), keypool.NewBreakerCell)
	s.in = append(s.in, cred, s.Breaker)
	s.Alone = &PoolPlan{Host: h, Slots: []*KeySlot{s}, Selection: pool.DefaultSelection}
	engine.Put(tx, SameOr(engine.Get[KeySlot](tx, id), s))
	return nil
}

// foldUpstream builds a slot from the host, account and credential rungs. Credential and account counters share the account's atomicity group, so one store call covers both.
func foldUpstream(tx *engine.Txn, cred *hostkey.HostKey, h *host.Host, acc *AccountPlan) *KeySlot {
	s := &KeySlot{Credential: cred, Host: h, Account: acc, budgetAt: -1}
	s.in = inputs{h, acc}
	group := "credential:" + cred.Meta.ID
	s.Upstream = [3]Rung{
		{Kind: limits.RungHost, SubjectID: "host:" + h.Meta.ID},
		{Kind: limits.RungAccount},
		{Kind: limits.RungCredential, SubjectID: "credential:" + cred.Meta.ID},
	}
	if acc != nil {
		s.Upstream[1].SubjectID = "account:" + acc.Account.Meta.ID
		group = s.Upstream[1].SubjectID
	}
	if cred.Meta.ID == hostkey.AnonIDPrefix+h.Meta.ID {
		s.Upstream[2].SubjectID = "" // the anonymous slot has no credential rung
	}
	var scoped []scopedLink
	for i := range s.Upstream {
		r := &s.Upstream[i]
		if r.SubjectID == "" {
			continue
		}
		rp := engine.Get[RungPolicy](tx, r.SubjectID)
		s.in = append(s.in, rp)
		if rp == nil {
			continue
		}
		r.Policy = rp.Rung.Policy
		if rp.Refused != nil {
			if s.Refused == nil {
				s.Refused = rp.Refused
			}
			continue
		}
		g := group
		if r.Kind == limits.RungHost {
			g = r.SubjectID
		}
		pid := r.Policy.Policy.Meta.ID
		for j := range r.Policy.RateLimits {
			l := &r.Policy.RateLimits[j]
			if l.Scoped {
				scoped = append(scoped, scopedLink{link: l, rung: r, policyID: pid, group: g})
				continue
			}
			for _, rule := range l.RateLimit.Spec.Rules {
				s.Whole = append(s.Whole, limits.UpstreamRule(r.Kind, l.RateLimit, rule, pid, r.SubjectID, "", g))
			}
		}
		for _, b := range r.Policy.Budgets {
			s.Whole = append(s.Whole, limits.BudgetCounter(r.Kind, b, pid, r.SubjectID, g))
			if !s.hasBudget {
				s.hasBudget, s.budgetAt = true, i
			}
		}
	}
	if len(scoped) > 0 {
		idx := engine.Get[CatalogIndex](tx, "")
		s.in = append(s.in, idx)
		s.ByModel = expandScoped(idx, h, scoped)
	}
	return s
}

// scopedLink is an upstream rate-limit link with a model scope, waiting to be expanded against the models its host serves.
type scopedLink struct {
	link     *LinkedRateLimit
	rung     *Rung
	policyID string
	group    string
}

// expandScoped gives each model the host serves the counters of every scoped link matching it: one bucket per matched model. A rule reached twice for one model (two overlapping links of one rate limit) counts once.
func expandScoped(idx *CatalogIndex, h *host.Host, links []scopedLink) map[string][]limits.Counter {
	if idx == nil {
		return nil
	}
	out := map[string][]limits.Counter{}
	for _, b := range idx.bindingsByHost[h.Meta.ID] {
		m, ok := idx.Snap.Model(b.Spec.ModelID)
		if !ok {
			continue
		}
		prov, _ := idx.Snap.ProviderSlug(m.Meta.Owner.ID)
		for _, sl := range links {
			if !sl.link.matches(prov, m.Meta.Name, h.Meta.Name) {
				continue
			}
			for _, rule := range sl.link.RateLimit.Spec.Rules {
				ct := limits.UpstreamRule(sl.rung.Kind, sl.link.RateLimit, rule, sl.policyID, sl.rung.SubjectID, m.Meta.ID, sl.group)
				if !hasCounter(out[m.Meta.ID], ct.ID) {
					out[m.Meta.ID] = append(out[m.Meta.ID], ct)
				}
			}
		}
	}
	return out
}

func hasCounter(cs []limits.Counter, id string) bool {
	for i := range cs {
		if cs[i].ID == id {
			return true
		}
	}
	return false
}

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
	pp := &PoolPlan{Pool: p, Host: h, Selection: p.Spec.Selection}
	if pp.Selection == "" {
		pp.Selection = pool.DefaultSelection
	}
	pp.in = inputs{p, h}
	if h.Spec.NoAuth {
		// A keyless host is reached through a pool linked on purpose, never by a wildcard: the pool is the explicit grant.
		if len(p.Spec.CredentialIDs) > 0 {
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueCredentialsIgnored, Source: src, Field: "spec.credentialIds", Message: "the host needs no key; requests go out without the pool's credentials"})
		}
		anon := foldUpstream(tx, hostkey.Anonymous(h.Meta.ID, h.Meta.Name), h, nil)
		pp.Slots = []*KeySlot{anon}
		pp.in = append(pp.in, anon.in...)
	} else {
		for i, cid := range p.Spec.CredentialIDs {
			s := engine.Get[KeySlot](tx, cid)
			if s == nil {
				if engine.Get[hostkey.HostKey](tx, cid) == nil {
					tx.Issue(refMissing(src, fmt.Sprintf("spec.credentialIds[%d]", i), engine.Ref{Kind: KindCredential, ID: cid}))
				}
				continue
			}
			if s.Host.Meta.ID != h.Meta.ID {
				tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueHostMismatch, Source: src, Field: fmt.Sprintf("spec.credentialIds[%d]", i), Target: engine.Ref{Kind: KindCredential, ID: cid}, Message: "the credential authenticates to another host; it is left out"})
				continue
			}
			pp.Slots = append(pp.Slots, s)
			pp.in = append(pp.in, s)
		}
	}
	pp.Rotation = cell.Acquire(c.cells, tx, keypool.RotationKey(id), keypool.NewRotation)
	pp.in = append(pp.in, pp.Rotation)
	engine.Put(tx, SameOr(engine.Get[PoolPlan](tx, id), pp))
	return nil
}

// OwnedCredentials is the credentials one owner scope holds ("user:<id>", "project:<id>", "team:<id>", "system:"), each as a pool of one in creation order: what a principal reaches when no caller rung links pools or credentials.
type OwnedCredentials struct {
	Pools []*PoolPlan
	in    inputs
}

func (o *OwnedCredentials) builtFrom() inputs { return o.in }

func ownerRef(o meta.Owner) string { return string(o.Kind) + ":" + o.ID }

func (c *controllers) ownedCredentials(tx *engine.Txn, owner string) error {
	oc := &OwnedCredentials{}
	for _, cred := range engine.Members[hostkey.HostKey](tx) {
		if s := engine.Get[KeySlot](tx, cred.Meta.ID); s != nil {
			oc.Pools = append(oc.Pools, s.Alone)
			oc.in = append(oc.in, s)
		}
	}
	engine.Put(tx, SameOr(engine.Get[OwnedCredentials](tx, owner), oc))
	return nil
}
