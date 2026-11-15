package control

import (
	"context"

	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
)

// enrichPolicyEditable fills Editable and EditBlockedBy for the caller with the edit guard's own checks. The bindings come from the catalog snapshot and the rows the checks read are cached for the answer; the probes are not audited, since they answer a read. Lists skip it: each row would run the binding checks of every binding using it.
func enrichPolicyEditable(d Deps) enrichFn[policy.Policy] {
	return func(ctx context.Context, p *policy.Policy) {
		if p == nil || d.Catalog == nil || d.Stores == nil {
			return
		}
		snap := d.Catalog.Current()
		if snap == nil {
			return
		}
		rd := d
		rd.Authz = unaudited(d.Authz)
		editable := policyChecks(rd).mayUpdate(ctx, p.Meta.ID, &p.Meta) == nil
		if editable && !maySetEveryPolicy(ctx, rd.Authz) {
			bindings := snap.AllPolicyBindings()
			c := memoRefs(rd)
			c.Rows.PolicyBindings = func(context.Context) ([]*policybinding.PolicyBinding, error) { return bindings, nil }
			ids, err := policyBlockers(ctx, rd, c, bindings, p.Meta.ID)
			if err != nil {
				return
			}
			if len(ids) > 0 {
				by := editBlockers(ctx, rd, c, ids)
				p.EditBlockedBy = &by
				editable = false
			}
		}
		p.Editable = &editable
	}
}

// skipPolicyListEnrich keeps the list from running enrichPolicyEditable once per row.
func skipPolicyListEnrich(context.Context, []*policy.Policy) {}
