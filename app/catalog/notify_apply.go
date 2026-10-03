package catalog

import (
	"context"
	"fmt"
	"strings"
)

// applyEvent fetches the row (for upserts) and calls the appropriate Apply* method.
func (l *Listener) applyEvent(ctx context.Context, e drainedEvent) error {
	switch e.Kind {
	case "provider":
		if e.Op == "delete" {
			return l.cat.ApplyProviderDelete(e.ID)
		}
		p, err := l.stores.provider.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if p == nil {
			return l.cat.ApplyProviderDelete(e.ID)
		}
		return l.cat.ApplyProviderUpsert(p)

	case "host":
		if e.Op == "delete" {
			return l.cat.ApplyHostDelete(e.ID)
		}
		h, err := l.stores.host.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if h == nil {
			return l.cat.ApplyHostDelete(e.ID)
		}
		return l.cat.ApplyHostUpsert(h)

	case "model":
		if e.Op == "delete" {
			return l.cat.ApplyModelDelete(e.ID)
		}
		m, err := l.stores.model.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if m == nil {
			return l.cat.ApplyModelDelete(e.ID)
		}
		return l.cat.ApplyModelUpsert(m)

	case "hostkey":
		if e.Op == "delete" {
			return l.cat.ApplyHostKeyDelete(e.ID)
		}
		k, err := l.stores.hostkey.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if k == nil {
			return l.cat.ApplyHostKeyDelete(e.ID)
		}
		return l.cat.ApplyHostKeyUpsert(k)

	case "ratelimit":
		if e.Op == "delete" {
			return l.cat.ApplyRateLimitDelete(e.ID)
		}
		r, err := l.stores.ratelimit.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if r == nil {
			return l.cat.ApplyRateLimitDelete(e.ID)
		}
		return l.cat.ApplyRateLimitUpsert(r)

	case "policy":
		if e.Op == "delete" {
			return l.cat.ApplyPolicyDelete(e.ID)
		}
		p, err := l.stores.policy.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if p == nil {
			return l.cat.ApplyPolicyDelete(e.ID)
		}
		return l.cat.ApplyPolicyUpsert(p)

	case "pricing":
		if e.Op == "delete" {
			return l.cat.ApplyPricingDelete(e.ID)
		}
		p, err := l.stores.pricing.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if p == nil {
			return l.cat.ApplyPricingDelete(e.ID)
		}
		return l.cat.ApplyPricingUpsert(p)

	case "relaykey":
		if e.Op == "delete" {
			return l.cat.ApplyKeyDelete(e.ID)
		}
		k, err := l.stores.key.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if k == nil {
			return l.cat.ApplyKeyDelete(e.ID)
		}
		return l.cat.ApplyKeyUpsert(k)

	case "team":
		if e.Op == "delete" {
			return l.cat.ApplyTeamDelete(e.ID)
		}
		t, err := l.stores.team.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if t == nil {
			return l.cat.ApplyTeamDelete(e.ID)
		}
		return l.cat.ApplyTeamUpsert(t)

	case "project":
		if e.Op == "delete" {
			return l.cat.ApplyProjectDelete(e.ID)
		}
		p, err := l.stores.project.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if p == nil {
			return l.cat.ApplyProjectDelete(e.ID)
		}
		return l.cat.ApplyProjectUpsert(p)

	case "serviceaccount":
		if e.Op == "delete" {
			return l.cat.ApplyServiceAccountDelete(e.ID)
		}
		sa, err := l.stores.serviceAccount.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if sa == nil {
			return l.cat.ApplyServiceAccountDelete(e.ID)
		}
		return l.cat.ApplyServiceAccountUpsert(sa)

	case "group":
		if e.Op == "delete" {
			return l.cat.ApplyGroupDelete(e.ID)
		}
		g, err := l.stores.group.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if g == nil {
			return l.cat.ApplyGroupDelete(e.ID)
		}
		return l.cat.ApplyGroupUpsert(g)

	case "role":
		if e.Op == "delete" {
			return l.cat.ApplyRoleDelete(e.ID)
		}
		r, err := l.stores.role.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if r == nil {
			return l.cat.ApplyRoleDelete(e.ID)
		}
		return l.cat.ApplyRoleUpsert(r)

	case "rolebinding":
		if e.Op == "delete" {
			return l.cat.ApplyRoleBindingDelete(e.ID)
		}
		b, err := l.stores.roleBinding.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if b == nil {
			return l.cat.ApplyRoleBindingDelete(e.ID)
		}
		return l.cat.ApplyRoleBindingUpsert(b)

	case "policybinding":
		if e.Op == "delete" {
			return l.cat.ApplyPolicyBindingDelete(e.ID)
		}
		b, err := l.stores.policyBinding.Get(ctx, e.ID)
		if err != nil {
			return err
		}
		if b == nil {
			return l.cat.ApplyPolicyBindingDelete(e.ID)
		}
		return l.cat.ApplyPolicyBindingUpsert(b)

	case "hostbinding":
		// PR1: bindings aren't consumed by routing yet, and the COW
		// incremental reconciler doesn't know this kind. Fall back to a
		// full reload — bindings change rarely (catalog edits), so the
		// cost is acceptable. PR2 adds incremental ApplyHostBinding* when
		// routing reads bindings.
		return l.cat.Reload(ctx)

	case "overlay":
		// Composite-key payload: id slot carries "kind|resource_id"
		// (see migration 000022's overlay_notify()).
		kind, resourceID, ok := strings.Cut(e.ID, "|")
		if !ok || l.stores.overlay == nil {
			return nil
		}
		if e.Op == "delete" {
			return l.cat.ApplyOverlayDelete(kind, resourceID)
		}
		o, err := l.stores.overlay.Get(ctx, kind, resourceID)
		if err != nil {
			return err
		}
		if o == nil {
			return l.cat.ApplyOverlayDelete(kind, resourceID)
		}
		return l.cat.ApplyOverlayUpsert(o)

	case "user":
		// Users are not catalog rows; only their token version reaches the
		// snapshot, and the map is cheap enough to rebuild whole.
		return l.cat.ReloadTokenVersions(ctx)

	case "settings":
		if e.Op == "delete" {
			l.cat.settings.applyDelete(e.ID)
			return nil
		}
		return l.cat.settings.applyUpsert(ctx, e.ID)
	}
	// parseEvent already refused anything not in validKinds, so reaching
	// here means a kind was added there without a case below — which would
	// otherwise be a silently ignored NOTIFY.
	return fmt.Errorf("catalog notify: no handler for kind %q", e.Kind)
}
