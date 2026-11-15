package catalogprune

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/modelref"
	"github.com/wyolet/relay/pkg/usage"
)

// loadCandidates lists every detached row of the pruned kinds and marks the
// ones an overlay targets.
func loadCandidates(ctx context.Context, tables map[string]table, ov Overlays) ([]*candidate, error) {
	var out []*candidate
	for _, kind := range deleteOrder {
		cs, err := tables[kind].load(ctx)
		if err != nil {
			return nil, err
		}
		out = append(out, cs...)
	}
	if ov == nil || len(out) == 0 {
		return out, nil
	}
	overlays, err := ov.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list overlays: %w", err)
	}
	targets := make(map[string]bool, len(overlays))
	for _, o := range overlays {
		targets[o.ResourceID] = true
	}
	for _, c := range out {
		c.overlay = targets[c.id]
	}
	return out, nil
}

// markNamedModels records on each model candidate the operator's policies
// naming it exactly in spec.models ("provider/model", with or without a
// host). A policy counts when a user, team or project owns it, or when it was
// edited. A clean catalog policy doesn't: the release that dropped the model
// updates it.
func markNamedModels(ctx context.Context, s Stores, cands []*candidate) error {
	var models []*candidate
	for _, c := range cands {
		if c.kind == "model" && c.owner.Kind == meta.OwnerProvider {
			models = append(models, c)
		}
	}
	if len(models) == 0 {
		return nil
	}
	policies, err := s.Policies.List(ctx)
	if err != nil {
		return fmt.Errorf("list policies: %w", err)
	}
	providers, err := s.Providers.List(ctx)
	if err != nil {
		return fmt.Errorf("list providers: %w", err)
	}
	providerName := make(map[string]string, len(providers))
	for _, p := range providers {
		providerName[p.Meta.ID] = p.Meta.Name
	}
	type modelKey struct{ provider, model string }
	named := map[modelKey][]Ref{}
	for _, p := range policies {
		if !p.Meta.Owner.Tenant() && !p.Meta.Dirty {
			continue
		}
		for _, raw := range p.Spec.Models {
			ref, err := modelref.Parse(raw)
			if err != nil || ref.ProviderWildcard || ref.ModelWildcard {
				continue
			}
			k := modelKey{ref.Provider, ref.Model}
			if n := named[k]; len(n) > 0 && n[len(n)-1].ID == p.Meta.ID {
				continue
			}
			named[k] = append(named[k], Ref{Kind: "policy", ID: p.Meta.ID, Name: p.Meta.Name})
		}
	}
	for _, c := range models {
		c.namedBy = named[modelKey{providerName[c.owner.ID], c.name}]
	}
	return nil
}

// gatherFacts answers the usage check and then the references of every row an
// earlier fact doesn't already keep, cheapest first. It returns why usage was
// unknown, empty when the store answered every check. After one failed usage
// read the rest are not tried: the store is down, and each try would wait.
func gatherFacts(ctx context.Context, cands []*candidate, opts Options, since time.Time) (string, error) {
	down := ""
	if opts.Usage == nil {
		down = errNoUsageStore.Error()
	}
	unknown := ""
	for _, c := range cands {
		if c.dirty || c.overlay || len(c.namedBy) > 0 {
			continue
		}
		if c.traffic != nil {
			c.usage = &usageFact{since: since, unknown: down}
			if down == "" {
				last, err := lastRequest(ctx, opts.Usage, *c.traffic, since)
				if err != nil {
					down = err.Error()
					c.usage.unknown = down
				}
				c.usage.last = last
			}
			if c.usage.unknown != "" {
				unknown = c.usage.unknown
				continue
			}
			if !c.usage.last.IsZero() {
				continue
			}
		}
		refs, overlay, err := references(ctx, opts.References, c.kind, c.id)
		if err != nil {
			return "", err
		}
		c.refs, c.overlay = refs, overlay
	}
	return unknown, nil
}

// lastRequest returns when the newest request q matches since the window
// start was made; zero for none.
func lastRequest(ctx context.Context, r UsageReader, q usage.EventQuery, since time.Time) (time.Time, error) {
	q.From, q.Limit = since, 1
	events, err := r.Events(ctx, q)
	if err != nil || len(events) == 0 {
		return time.Time{}, err
	}
	return events[0].Timestamp, nil
}

// references lists what references a row; an overlay among them is reported
// apart, as the overlay reason.
func references(ctx context.Context, fn func(context.Context, string, string) ([]Ref, error), kind, id string) ([]Ref, bool, error) {
	all, err := fn(ctx, kind, id)
	if err != nil {
		return nil, false, fmt.Errorf("references of %s %s: %w", kind, id, err)
	}
	refs, overlay := all[:0:0], false
	for _, r := range all {
		if r.Kind == "overlay" {
			overlay = true
			continue
		}
		refs = append(refs, r)
	}
	return refs, overlay, nil
}

// applyDecisions deletes the rows decided prune, kind by kind in deleteOrder.
// Each row's references are read again first, since a row created after the
// decision may point at it, and the delete is conditional on the version
// judged. A row either check refuses turns keep.
func applyDecisions(ctx context.Context, tables map[string]table, rows []Decision, refsOf func(context.Context, string, string) ([]Ref, error)) error {
	for _, kind := range deleteOrder {
		del := tables[kind].del
		for i := range rows {
			d := &rows[i]
			if d.Kind != kind || d.Decision != Prune {
				continue
			}
			refs, overlay, err := references(ctx, refsOf, kind, d.ID)
			if err != nil {
				return err
			}
			switch {
			case overlay:
				d.Decision, d.Reason = Keep, "overlay"
				continue
			case len(refs) > 0:
				d.Decision, d.Reason = Keep, referencedBy(refs, "")
				continue
			}
			err = del(ctx, d.ID, d.version)
			if errors.Is(err, meta.ErrStaleResourceVersion) {
				d.Decision, d.Reason = Keep, "changed during the prune"
				continue
			}
			if err != nil {
				return fmt.Errorf("delete %s %q: %w", kind, d.Name, err)
			}
			d.Deleted = true
		}
	}
	return nil
}
