package plan

import (
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/binding"
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

// Rung is one level of the ladder as a plan holds it: the account and the policy bound to it. A nil Policy does not constrain.
type Rung struct {
	Kind limits.Rung
	// SubjectID is the account, as "account:<id>"; empty for a rung the request does not climb.
	SubjectID string
	Account   *account.Account
	// ScopeID is the team or project id of a team or project rung, and of the lane's own scope; ID returns it.
	ScopeID string
	// Name is the slug of the team, project or service account the rung stands for, for usage rows; empty otherwise.
	Name   string
	Policy *ExpandedPolicy
}

// AccountPlan is one provider account and its cooldown cell, shared by every credential of the account.
type AccountPlan struct {
	Account *account.Account
	// GroupedByRelay is the account's Spec.GroupedByRelay: its keys were grouped by owner and host, not declared one provider org, so an upstream 429 stays with the key.
	GroupedByRelay bool
	Cooldown       *keypool.CooldownCell
	in             inputs
}

func (a *AccountPlan) builtFrom() inputs { return a.in }

func (c *controllers) accountPlan(tx *engine.Txn, id string) error {
	acc := engine.Get[account.Account](tx, id)
	// A shared account holds no keys: its slots carry the real account's plan.
	if acc == nil || !acc.IsProvider() || acc.IsShared() {
		engine.Drop(tx)
		return nil
	}
	cd := cell.Acquire(c.cells, tx, keypool.CooldownCellKey(id), keypool.NewCooldownCell)
	// Built from the id, host and grouping mark alone, so a learned org id written onto the row keeps this plan and everything that reads it.
	ap := &AccountPlan{Account: acc, GroupedByRelay: acc.Spec.GroupedByRelay, Cooldown: cd, in: inputs{acc.Meta.ID, acc.Spec.HostID, acc.Spec.GroupedByRelay, cd}}
	engine.Put(tx, SameOr(engine.Get[AccountPlan](tx, id), ap))
	return nil
}

// KeySlot is one credential with the upstream half of the ladder folded: its host and provider account, the policy bound to the account, the counters it imposes, and the breaker cell. One KeySlot is shared by every pool holding the credential.
type KeySlot struct {
	Credential *hostkey.HostKey
	Host       *host.Host
	Account    *AccountPlan         // nil when the credential names no account
	Breaker    *keypool.BreakerCell // nil for a keyless host's anonymous slot
	Upstream   Rung                 // the provider account's
	// Via is the shared account the slot is reached through, whose policy narrows the real account's; SubjectID is empty on the real account's own slot.
	Via Rung
	// OnlyFor is the one lane holder ("serviceaccount:<id>") a service account's share serves; empty serves every lane reaching the slot.
	OnlyFor string
	// Refused is set when the account's policy is disabled or its binding ambiguous: the credential then serves nothing, rather than serving unmetered.
	Refused *Refusal
	// Whole holds the account's rules with no model scope and its budgets; ByModel the scoped rules, expanded per model id. A counter whose link is not shared counts per lane: the route fill renders it for the caller's lane.
	Whole   []slotCounter
	ByModel map[string][]slotCounter
	// Alone is the credential as a pool of one, for reach by ownership.
	Alone     *PoolPlan
	hasBudget bool
	in        inputs
}

// slotCounter is an upstream counter and whether it counts once per lane.
type slotCounter struct {
	limits.Counter
	perLane bool
}

func (s *KeySlot) builtFrom() inputs { return s.in }

// rejects returns the rung whose grant leaves out the model at this slot's host, nil when both allow it.
func (s *KeySlot) rejects(providerSlug string, m *model.Model, hostSlug string) *Rung {
	for _, r := range [2]*Rung{&s.Upstream, &s.Via} {
		if p := r.Policy; p != nil && p.Grant != nil && !p.Grant.Allows(providerSlug, m, hostSlug) {
			return r
		}
	}
	return nil
}

// CountsOnAccount reports whether c counts on the real provider account of the slot, rather than on a shared account it is reached through.
func (s *KeySlot) CountsOnAccount(c *limits.Counter) bool {
	return s.Via.SubjectID == "" || c.SubjectID != s.Via.SubjectID
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

// foldUpstream builds a slot from its provider account's rung. A credential has no rung of its own, and a host is no binding subject, so the account's tier policy is the whole upstream half.
func foldUpstream(tx *engine.Txn, cred *hostkey.HostKey, h *host.Host, acc *AccountPlan) *KeySlot {
	s := &KeySlot{Credential: cred, Host: h, Account: acc, Upstream: Rung{Kind: limits.RungAccount}}
	s.in = inputs{h, acc}
	if acc == nil {
		return s
	}
	r := &s.Upstream
	r.SubjectID, r.Account = accountSubject(acc.Account.Meta.ID), acc.Account
	var counts rungCounts
	counts, s.Refused = foldAccountRung(tx, h, r, r.SubjectID, &s.in)
	s.Whole, s.ByModel, s.hasBudget = counts.whole, counts.byModel, counts.hasBudget
	return s
}

// rungCounts are one provider account rung's counters as a slot carries them.
type rungCounts struct {
	whole     []slotCounter
	byModel   map[string][]slotCounter
	hasBudget bool
}

// foldAccountRung reads the policy bound to r's account into r and returns its counters, counted in atomicity group group, or the refusal its binding imposes.
func foldAccountRung(tx *engine.Txn, h *host.Host, r *Rung, group string, in *inputs) (rungCounts, *Refusal) {
	var out rungCounts
	rp := engine.Get[RungPolicy](tx, r.SubjectID)
	*in = append(*in, rp)
	if rp == nil {
		return out, nil
	}
	r.Policy = rp.Rung.Policy
	if rp.Refused != nil {
		return out, rp.Refused
	}
	pid := r.Policy.Policy.Meta.ID
	var scoped []*LinkedRateLimit
	for j := range r.Policy.RateLimits {
		l := &r.Policy.RateLimits[j]
		if l.Scoped {
			scoped = append(scoped, l)
			continue
		}
		for _, rule := range l.RateLimit.Spec.Rules {
			out.whole = append(out.whole, slotCounter{limits.UpstreamRule(r.Kind, l.RateLimit, rule, pid, r.SubjectID, "", group), !l.Shared})
		}
	}
	for _, b := range r.Policy.Budgets {
		out.whole = append(out.whole, slotCounter{limits.BudgetCounter(r.Kind, b.Budget, pid, r.SubjectID, group), !b.Shared})
		out.hasBudget = true
	}
	if len(scoped) > 0 {
		idx := engine.Get[CatalogIndex](tx, "")
		*in = append(*in, idx)
		out.byModel = expandScoped(idx, h, r, pid, scoped, group)
	}
	return out, nil
}

// refuses reports why the slot may not serve model m through binding b at its host, and the fill step that refuses it; a nil refusal means it may.
func (s *KeySlot) refuses(providerSlug string, m *model.Model, b *binding.Binding, hostSlug string, priced bool) (int, *Refusal) {
	if s.Refused != nil {
		return stepUpstream, s.Refused
	}
	if r := s.rejects(providerSlug, m, hostSlug); r != nil {
		whose := "the provider account's policy"
		if r == &s.Via {
			whose = "the policy of the shared account " + r.Account.Meta.Name
		}
		return stepUpstream, rungRefusal(CodeModelNotAllowed, r, r.Policy.Policy.Meta.ID, m.Meta.Name+" is not allowed by "+whose)
	}
	if !priced && s.hasBudget {
		r := &s.Upstream
		if r.Policy == nil || len(r.Policy.Budgets) == 0 {
			r = &s.Via
		}
		return stepUnpriced, rungRefusal(CodeUnpricedUnderBudget, r, r.Policy.Policy.Meta.ID, "binding "+b.Meta.Name+" has no pricing and a budget applies")
	}
	return 0, nil
}

// expandScoped gives each model the host serves the counters of every scoped link matching it: one bucket per matched model. A rule reached twice for one model (two overlapping links of one rate limit) counts once.
func expandScoped(idx *CatalogIndex, h *host.Host, r *Rung, policyID string, links []*LinkedRateLimit, group string) map[string][]slotCounter {
	if idx == nil {
		return nil
	}
	out := map[string][]slotCounter{}
	for _, b := range idx.bindingsByHost[h.Meta.ID] {
		m, ok := idx.Snap.Model(b.Spec.ModelID)
		if !ok {
			continue
		}
		prov, _ := idx.Snap.ProviderSlug(m.Meta.Owner.ID)
		for _, l := range links {
			if !l.matches(prov, m.Meta.Name, h.Meta.Name) {
				continue
			}
			for _, rule := range l.RateLimit.Spec.Rules {
				ct := slotCounter{limits.UpstreamRule(r.Kind, l.RateLimit, rule, policyID, r.SubjectID, m.Meta.ID, group), !l.Shared}
				if !hasCounter(out[m.Meta.ID], ct) {
					out[m.Meta.ID] = append(out[m.Meta.ID], ct)
				}
			}
		}
	}
	return out
}

func hasCounter(cs []slotCounter, c slotCounter) bool {
	for i := range cs {
		if cs[i].ID == c.ID && cs[i].perLane == c.perLane {
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
		if len(p.Spec.CredentialIDs) > 0 || len(p.Spec.AccountIDs) > 0 {
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueCredentialsIgnored, Source: src, Field: "spec.credentialIds", Message: "the host needs no key; requests go out without the pool's credentials and accounts"})
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

// poolSlots fills pp with the pool's slots in prioritized order: every key of each listed account, in account order and each account's keys in creation order, then the listed credentials; a key reached twice the same way keeps its first place. Two shares reaching one key stay apart, as each lane may use only one of them.
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
		if a.IsShared() {
			// A shared account whose real account is gone carries the issue itself.
			sa := engine.Get[SharedAccount](tx, aid)
			pp.in = append(pp.in, sa)
			if sa != nil {
				for _, s := range sa.Slots {
					add(s)
				}
			}
			continue
		}
		oc := engine.Get[OwnedCredentials](tx, accountSubject(aid))
		pp.in = append(pp.in, oc)
		if oc != nil {
			for _, one := range oc.Pools {
				add(one.Slots[0])
			}
		}
	}
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
		pp.in = append(pp.in, s)
		add(s)
	}
}

// OwnedCredentials is the credentials one owner scope holds ("user:<id>", "project:<id>", "team:<id>", "system:"), or one provider account holds ("account:<id>"), each as a pool of one in creation order: what a lane reaches when no caller rung links pools.
type OwnedCredentials struct {
	Pools []*PoolPlan
	in    inputs
}

func (o *OwnedCredentials) builtFrom() inputs { return o.in }

func ownerRef(o meta.Owner) string { return string(o.Kind) + ":" + o.ID }

// ownedCredentialKeys is a credential's Watch key: its owner scope and, when it names one, its provider account.
func ownedCredentialKeys(k *hostkey.HostKey) []string {
	if k.Spec.AccountID == "" {
		return []string{ownerRef(k.Meta.Owner)}
	}
	return []string{ownerRef(k.Meta.Owner), accountSubject(k.Spec.AccountID)}
}

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
