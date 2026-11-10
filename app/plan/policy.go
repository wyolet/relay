package plan

import (
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/modelref"
)

// CatalogIndex is the catalog a route fill resolves against: a snapshot of the catalog kinds alone, with each host's enabled bindings for expanding scoped upstream rules.
type CatalogIndex struct {
	Snap           *appcatalog.Snapshot
	bindingsByHost map[string][]*binding.Binding
	// unservable is what policy checks report model_unservable from.
	unservable []unservableModel
}

func (c *controllers) catalogIndex(tx *engine.Txn, _ string) error {
	models := engine.Members[model.Model](tx)
	snap := appcatalog.BuildCatalog(
		engine.Members[provider.Provider](tx), engine.Members[host.Host](tx), models,
		engine.Members[pricing.Pricing](tx), engine.Members[binding.Binding](tx), engine.Members[overlay.Overlay](tx),
	)
	idx := &CatalogIndex{Snap: snap, bindingsByHost: map[string][]*binding.Binding{}}
	for _, b := range snap.AllBindings() {
		if b.IsEnabled() {
			idx.bindingsByHost[b.Spec.HostID] = append(idx.bindingsByHost[b.Spec.HostID], b)
		}
	}
	idx.unservable = unservableModels(snap, models)
	engine.Put(tx, idx)
	return nil
}

// ExpandedPolicy is one policy with its links read: model refs parsed into a Grant, rate limits and budgets resolved, and every missing link dropped with a ref_missing issue on the policy. Every rung binding the policy shares it.
type ExpandedPolicy struct {
	Policy *policy.Policy
	// Grant is nil when the policy names no models: its rung then does not narrow models.
	Grant      *Grant
	RateLimits []LinkedRateLimit
	Budgets    []LinkedBudget
	in         inputs
}

func (p *ExpandedPolicy) builtFrom() inputs { return p.in }

// links reports whether the policy links pools, which makes its rung decide reach.
func (p *ExpandedPolicy) links() bool {
	return p != nil && len(p.Policy.Spec.PoolIDs) > 0
}

// Grant is a policy's model list, parsed once. An entry that cannot match is dropped, so a list of only such entries grants nothing rather than everything.
type Grant struct {
	refs []modelref.Ref
}

// Allows reports whether the grant covers model m of provider providerSlug served at host hostSlug.
func (g *Grant) Allows(providerSlug string, m *model.Model, hostSlug string) bool {
	return appcatalog.RefsGrant(g.refs, providerSlug, m.Meta.Name, hostSlug, false)
}

// LinkedRateLimit is one rateLimits entry with its rate limit read. A scoped entry applies only to models its Scope matches; one whose refs all failed to parse matches no model. Shared counts once for the bound account; otherwise once per lane under it.
type LinkedRateLimit struct {
	RateLimit *ratelimit.RateLimit
	Scoped    bool
	Scope     []modelref.Ref
	// Models is the entry's model list as written, for reads that show it.
	Models []string
	Shared bool
}

// LinkedBudget is one budgets entry with its budget read.
type LinkedBudget struct {
	Budget *budget.Budget
	Shared bool
}

func (l *LinkedRateLimit) matches(providerSlug, modelSlug, hostSlug string) bool {
	if !l.Scoped {
		return true
	}
	for _, r := range l.Scope {
		if r.Matches(providerSlug, modelSlug, hostSlug) {
			return true
		}
	}
	return false
}

func (c *controllers) expandedPolicy(tx *engine.Txn, id string) error {
	p := engine.Get[policy.Policy](tx, id)
	if p == nil {
		engine.Drop(tx)
		return nil
	}
	src := engine.Ref{Kind: KindPolicy, ID: id}
	ep := &ExpandedPolicy{Policy: p}
	in := inputs{p}
	if len(p.Spec.Models) > 0 {
		ep.Grant = &Grant{refs: parseRefs(tx, src, "spec.models", p.Spec.Models)}
	}
	for i, l := range p.Spec.RateLimits {
		field := fmt.Sprintf("spec.rateLimits[%d]", i)
		rl := engine.Get[ratelimit.RateLimit](tx, l.RateLimitID)
		if rl == nil {
			tx.Issue(refMissing(src, field, engine.Ref{Kind: KindRateLimit, ID: l.RateLimitID}))
			continue
		}
		in = append(in, rl)
		if !rl.IsEnabled() {
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueRateLimitDisabled, Source: src, Field: field, Target: engine.Ref{Kind: KindRateLimit, ID: rl.Meta.ID},
				Message: "rate limit " + rl.Meta.Name + " is disabled; it does not apply to this policy"})
			continue
		}
		ep.RateLimits = append(ep.RateLimits, LinkedRateLimit{RateLimit: rl, Scoped: len(l.Models) > 0, Scope: parseRefs(tx, src, field+".models", l.Models), Models: l.Models, Shared: l.IsShared()})
	}
	for i, l := range p.Spec.Budgets {
		b := engine.Get[budget.Budget](tx, l.BudgetID)
		if b == nil {
			tx.Issue(refMissing(src, fmt.Sprintf("spec.budgets[%d]", i), engine.Ref{Kind: KindBudget, ID: l.BudgetID}))
			continue
		}
		in = append(in, b)
		ep.Budgets = append(ep.Budgets, LinkedBudget{Budget: b, Shared: l.IsShared()})
	}
	// Pools are read as plans by the caller fold; here only their presence is checked, so the issue sits on the policy once.
	hosts := map[string]bool{}
	for i, pid := range p.Spec.PoolIDs {
		if pl := engine.Get[pool.Pool](tx, pid); pl != nil {
			hosts[pl.Spec.HostID] = true
		} else {
			tx.Issue(refMissing(src, fmt.Sprintf("spec.poolIds[%d]", i), engine.Ref{Kind: KindPool, ID: pid}))
		}
	}
	if len(ep.Budgets) > 0 {
		warnUnpriced(tx, src, ep, hosts)
	}
	ep.in = in
	engine.Put(tx, SameOr(engine.Get[ExpandedPolicy](tx, id), ep))
	return nil
}

// parseRefs parses a model list, reporting each entry that cannot match anything.
func parseRefs(tx *engine.Txn, src engine.Ref, field string, raw []string) []modelref.Ref {
	refs := make([]modelref.Ref, 0, len(raw))
	for i, s := range raw {
		f := fmt.Sprintf("%s[%d]", field, i)
		if strings.HasPrefix(s, "group:") {
			tx.Issue(engine.Issue{Severity: engine.SeverityWarning, Code: IssueModelGroupUnsupported, Source: src, Field: f, Message: "model groups are not resolved yet; " + s + " matches no model"})
			continue
		}
		r, err := modelref.Parse(s)
		if err != nil {
			tx.Issue(engine.Issue{Severity: engine.SeverityError, Code: IssueModelRefInvalid, Source: src, Field: f, Message: err.Error()})
			continue
		}
		refs = append(refs, r)
	}
	return refs
}

// warnUnpriced reports the bindings a budgeted policy allows that have no pricing: requests for them are refused while the budget applies. Limited to the hosts of the pools the policy links, when it links any.
func warnUnpriced(tx *engine.Txn, src engine.Ref, ep *ExpandedPolicy, hosts map[string]bool) {
	idx := engine.Get[CatalogIndex](tx, "")
	if idx == nil {
		return
	}
	n, first := 0, ""
	for hostID, bs := range idx.bindingsByHost {
		if len(hosts) > 0 && !hosts[hostID] {
			continue
		}
		h, ok := idx.Snap.Host(hostID)
		if !ok {
			continue
		}
		for _, b := range bs {
			m, ok := idx.Snap.Model(b.Spec.ModelID)
			if !ok || !m.IsEnabled() {
				continue
			}
			prov, _ := idx.Snap.ProviderSlug(m.Meta.Owner.ID)
			if ep.Grant != nil && !ep.Grant.Allows(prov, m, h.Meta.Name) {
				continue
			}
			if _, priced := idx.Snap.PricingForBinding(b); priced {
				continue
			}
			if n++; first == "" || b.Meta.Name < first {
				first = b.Meta.Name
			}
		}
	}
	if n > 0 {
		tx.Issue(engine.Issue{
			Severity: engine.SeverityWarning, Code: CodeUnpricedUnderBudget, Source: src, Field: "spec.budgets",
			Message: fmt.Sprintf("%d bindings this policy allows have no pricing (e.g. %s); requests for them are refused while a budget applies", n, first),
		})
	}
}

// RungPolicy is the policy bound to one account ("account:<id>"), or the refusal the account's rung imposes when its policy is disabled or its binding ambiguous.
type RungPolicy struct {
	Rung    Rung
	Binding *policybinding.PolicyBinding
	Refused *Refusal
	// unusable are the pool and credential ids the policy links that the account's scope may not use, sorted; the caller fold skips them.
	unusable []string
	in       inputs
}

func (r *RungPolicy) builtFrom() inputs { return r.in }

// accountSubject is the subject key of an account: what bindings, rung policies and counters name it by.
func accountSubject(id string) string { return "account:" + id }

// rungOf is the ladder rung an account sits at: a team's or project's own account its scope's rung, a provider account the upstream rung, any other the lane.
func rungOf(a *account.Account) limits.Rung {
	switch a.DerivedKind() {
	case account.KindTeam:
		return limits.RungTeam
	case account.KindProject:
		return limits.RungProject
	case account.KindProvider:
		return limits.RungAccount
	}
	return limits.RungLane
}

// bindingSubjects is a policy binding's Watch key: the accounts it binds, none while it is disabled.
func bindingSubjects(b *policybinding.PolicyBinding) []string {
	if !b.IsEnabled() {
		return nil
	}
	out := make([]string, 0, len(b.Spec.AccountIDs))
	for _, id := range b.Spec.AccountIDs {
		out = append(out, accountSubject(id))
	}
	return out
}

func (c *controllers) rungPolicy(tx *engine.Txn, subject string) error {
	_, id, _ := strings.Cut(subject, ":")
	acc := engine.Get[account.Account](tx, id)
	if acc == nil {
		engine.Drop(tx)
		return nil
	}
	bs := c.bindingsPermitted(tx, subject, bindingsInScope(tx, acc, engine.Members[policybinding.PolicyBinding](tx)))
	rp := bindRung(tx, acc, bs)
	if rp == nil {
		engine.Drop(tx)
		return nil
	}
	engine.Put(tx, SameOr(engine.Get[RungPolicy](tx, subject), rp))
	return nil
}

// bindRung folds the bindings naming one account into its rung; nil when none binds a policy that exists.
func bindRung(tx *engine.Txn, acc *account.Account, bs []*policybinding.PolicyBinding) *RungPolicy {
	subject := accountSubject(acc.Meta.ID)
	rp := &RungPolicy{Rung: Rung{Kind: rungOf(acc), SubjectID: subject}}
	// The account's kind and owner shape the rung; its other fields (a learned org id, a rename) do not, so they rebuild nothing.
	read := inputs{acc.DerivedKind(), acc.Meta.Owner}
	switch len(bs) {
	case 0:
		return nil
	case 1:
		b := bs[0]
		ep := engine.Get[ExpandedPolicy](tx, b.Spec.PolicyID)
		if ep == nil {
			tx.Issue(parentMissing(engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, "spec.policyId", engine.Ref{Kind: KindPolicy, ID: b.Spec.PolicyID}))
			return nil
		}
		rp.Binding, rp.Rung.Policy, rp.in = b, ep, append(read, b, ep)
		if !acc.IsProvider() && ep.links() {
			if rp.unusable = unusableLinks(tx, ep.Policy, acc.Meta.Owner, b); len(rp.unusable) > 0 {
				rp.in = append(rp.in, strings.Join(rp.unusable, ","))
			}
		}
		if !ep.Policy.IsEnabled() {
			rp.Refused = rungRefusal(CodePolicyDisabled, &rp.Rung, ep.Policy.Meta.ID, "policy "+ep.Policy.Meta.Name+" is disabled")
		}
	default:
		// Taking neither binding would widen the rung; refusing keeps every rung a narrowing.
		rp.Refused = rungRefusal(CodeDuplicateBinding, &rp.Rung, "", fmt.Sprintf("%d policy bindings name %s", len(bs), subject))
		rp.in = read
		for _, b := range bs {
			rp.in = append(rp.in, b)
			tx.Issue(engine.Issue{
				Severity: engine.SeverityError, Code: CodeDuplicateBinding, Source: engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, Field: "spec.accountIds",
				Message: fmt.Sprintf("%s is bound by %d policy bindings; the rung refuses until one remains", subject, len(bs)),
			})
		}
	}
	return rp
}
