package plan

import (
	"fmt"
	"strings"

	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/budget"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/modelref"
	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/engine"
)

// CatalogIndex is the catalog a route fill resolves against: a snapshot of the catalog kinds alone, with each host's enabled bindings for expanding scoped upstream rules.
type CatalogIndex struct {
	Snap           *appcatalog.Snapshot
	bindingsByHost map[string][]*binding.Binding
}

func (c *controllers) catalogIndex(tx *engine.Txn, _ string) error {
	snap := appcatalog.BuildCatalog(
		engine.Members[provider.Provider](tx), engine.Members[host.Host](tx), engine.Members[model.Model](tx),
		engine.Members[pricing.Pricing](tx), engine.Members[binding.Binding](tx), engine.Members[overlay.Overlay](tx),
	)
	idx := &CatalogIndex{Snap: snap, bindingsByHost: map[string][]*binding.Binding{}}
	for _, b := range snap.AllBindings() {
		if b.IsEnabled() {
			idx.bindingsByHost[b.Spec.HostID] = append(idx.bindingsByHost[b.Spec.HostID], b)
		}
	}
	engine.Put(tx, idx)
	return nil
}

// ExpandedPolicy is one policy with its links read: model refs parsed into a Grant, rate limits and budgets resolved, and every missing link dropped with a ref_missing issue on the policy. Every rung binding the policy shares it.
type ExpandedPolicy struct {
	Policy *policy.Policy
	// Grant is nil when the policy names no models: its rung then does not narrow models.
	Grant      *Grant
	RateLimits []LinkedRateLimit
	Budgets    []*budget.Budget
	in         inputs
}

func (p *ExpandedPolicy) builtFrom() inputs { return p.in }

// links reports whether the policy links pools or credentials, which makes its rung decide reach.
func (p *ExpandedPolicy) links() bool {
	return p != nil && (len(p.Policy.Spec.PoolIDs) > 0 || len(p.Policy.Spec.CredentialIDs) > 0)
}

// Grant is a policy's model list, parsed once. An entry that cannot match is dropped, so a list of only such entries grants nothing rather than everything.
type Grant struct {
	refs              []modelref.Ref
	ids               map[string]struct{} // literal model ids from the older grant field
	includeDeprecated bool
}

// Allows reports whether the grant covers model m of provider providerSlug served at host hostSlug. Wildcards skip deprecated models unless the policy includes them; a ref naming the model grants it either way.
func (g *Grant) Allows(providerSlug string, m *model.Model, hostSlug string) bool {
	if _, ok := g.ids[m.Meta.ID]; ok {
		return true
	}
	return appcatalog.RefsGrant(g.refs, providerSlug, m.Meta.Name, hostSlug, appcatalog.ModelDeprecated(m) && !g.includeDeprecated)
}

// LinkedRateLimit is one rateLimits entry with its rate limit read. A scoped entry applies only to models its Scope matches; one whose refs all failed to parse matches no model.
type LinkedRateLimit struct {
	RateLimit *ratelimit.RateLimit
	Scoped    bool
	Scope     []modelref.Ref
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
	if len(p.Spec.Models) > 0 || len(p.Spec.ModelIDs) > 0 {
		g := &Grant{includeDeprecated: p.Spec.IncludeDeprecated, refs: parseRefs(tx, src, "spec.models", p.Spec.Models)}
		if len(p.Spec.ModelIDs) > 0 {
			g.ids = make(map[string]struct{}, len(p.Spec.ModelIDs))
			for _, mid := range p.Spec.ModelIDs {
				g.ids[mid] = struct{}{}
			}
		}
		ep.Grant = g
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
			continue
		}
		ep.RateLimits = append(ep.RateLimits, LinkedRateLimit{RateLimit: rl, Scoped: len(l.Models) > 0, Scope: parseRefs(tx, src, field+".models", l.Models)})
	}
	for i, bid := range p.Spec.BudgetIDs {
		b := engine.Get[budget.Budget](tx, bid)
		if b == nil {
			tx.Issue(refMissing(src, fmt.Sprintf("spec.budgetIds[%d]", i), engine.Ref{Kind: KindBudget, ID: bid}))
			continue
		}
		in = append(in, b)
		ep.Budgets = append(ep.Budgets, b)
	}
	// Pools and credentials are read as plans by the caller fold; here only their presence is checked, so the issue sits on the policy once.
	hosts := map[string]bool{}
	for i, pid := range p.Spec.PoolIDs {
		if pl := engine.Get[pool.Pool](tx, pid); pl != nil {
			hosts[pl.Spec.HostID] = true
		} else {
			tx.Issue(refMissing(src, fmt.Sprintf("spec.poolIds[%d]", i), engine.Ref{Kind: KindPool, ID: pid}))
		}
	}
	for i, cid := range p.Spec.CredentialIDs {
		if cr := engine.Get[hostkey.HostKey](tx, cid); cr != nil {
			hosts[cr.Spec.HostID] = true
		} else {
			tx.Issue(refMissing(src, fmt.Sprintf("spec.credentialIds[%d]", i), engine.Ref{Kind: KindCredential, ID: cid}))
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

// warnUnpriced reports the bindings a budgeted policy allows that have no pricing: requests for them are refused while the budget applies. Limited to the hosts of the pools and credentials the policy links, when it links any.
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
			Severity: engine.SeverityWarning, Code: CodeUnpricedUnderBudget, Source: src, Field: "spec.budgetIds",
			Message: fmt.Sprintf("%d bindings this policy allows have no pricing (e.g. %s); requests for them are refused while a budget applies", n, first),
		})
	}
}

// RungPolicy is the policy bound at one rung (a subject such as "team:<id>"), or the refusal the rung imposes when its policy is disabled or its binding ambiguous.
type RungPolicy struct {
	Rung    Rung
	Binding *policybinding.PolicyBinding
	Refused *Refusal
	in      inputs
}

func (r *RungPolicy) builtFrom() inputs { return r.in }

// rungOf maps a binding subject to its ladder rung; group and key subjects are not rungs.
func rungOf(subject string) (limits.Rung, bool) {
	kind, _, _ := strings.Cut(subject, ":")
	switch kind {
	case "team":
		return limits.RungTeam, true
	case "project":
		return limits.RungProject, true
	case "user", "serviceaccount":
		return limits.RungPrincipal, true
	case "host":
		return limits.RungHost, true
	case "account":
		return limits.RungAccount, true
	case "credential":
		return limits.RungCredential, true
	}
	return "", false
}

// bindingSubjects is a policy binding's Watch key: the subjects it binds, none while it is disabled.
func bindingSubjects(b *policybinding.PolicyBinding) []string {
	if !b.IsEnabled() {
		return nil
	}
	out := make([]string, 0, len(b.Spec.Subjects))
	for i := range b.Spec.Subjects {
		out = append(out, b.Spec.Subjects[i].Key())
	}
	return out
}

func (c *controllers) rungPolicy(tx *engine.Txn, subject string) error {
	bs := engine.Members[policybinding.PolicyBinding](tx)
	kind, ok := rungOf(subject)
	if !ok {
		for _, b := range bs {
			tx.Issue(engine.Issue{
				Severity: engine.SeverityWarning, Code: IssueSubjectInvalid, Source: engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, Field: "spec.subjects",
				Message: subject + " is not a rung of the policy ladder (team, project, user, service account, host, account, credential); the binding is ignored for it",
			})
		}
		engine.Drop(tx)
		return nil
	}
	rp := &RungPolicy{Rung: Rung{Kind: kind, SubjectID: subject}}
	switch len(bs) {
	case 0:
		engine.Drop(tx)
		return nil
	case 1:
		b := bs[0]
		ep := engine.Get[ExpandedPolicy](tx, b.Spec.PolicyID)
		if ep == nil {
			tx.Issue(parentMissing(engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, "spec.policyId", engine.Ref{Kind: KindPolicy, ID: b.Spec.PolicyID}))
			engine.Drop(tx)
			return nil
		}
		rp.Binding, rp.Rung.Policy, rp.in = b, ep, inputs{b, ep}
		if !ep.Policy.IsEnabled() {
			rp.Refused = rungRefusal(CodePolicyDisabled, &rp.Rung, ep.Policy.Meta.ID, "policy "+ep.Policy.Meta.Name+" is disabled")
		}
	default:
		// Taking neither binding would widen the rung; refusing keeps every rung a narrowing.
		rp.Refused = rungRefusal(CodeDuplicateBinding, &rp.Rung, "", fmt.Sprintf("%d policy bindings name %s", len(bs), subject))
		for _, b := range bs {
			rp.in = append(rp.in, b)
			tx.Issue(engine.Issue{
				Severity: engine.SeverityError, Code: CodeDuplicateBinding, Source: engine.Ref{Kind: KindPolicyBinding, ID: b.Meta.ID}, Field: "spec.subjects",
				Message: fmt.Sprintf("%s is bound by %d policy bindings; the rung refuses until one remains", subject, len(bs)),
			})
		}
	}
	engine.Put(tx, SameOr(engine.Get[RungPolicy](tx, subject), rp))
	return nil
}
