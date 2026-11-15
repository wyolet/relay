// Package catalogprune deletes the catalog rows a catalog release stopped
// shipping once nothing holds them. A versioned reseed only marks such rows
// detached (meta.CatalogDetachedAt); a prune deletes a detached row that is
// not operator-edited, carries no overlay, is referenced by nothing but other
// detached rows deleted with it, and, for a model or a host binding, served
// no request inside the idle window. A model an operator's policy names in
// spec.models also stays. Every other detached row stays, and the next prune
// judges it again.
//
// What references a row is the caller's answer (the control API's delete
// blockers and dropped links), so a prune and an admin delete agree on what
// holds a row. Out of scope: rows that never came from the catalog, usage
// history (never touched), and anything on the request path.
package catalogprune

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wyolet/relay/app/overlay"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/pkg/usage"
)

// deleteOrder is the kinds a prune judges, in the order it deletes them:
// each kind after every kind whose rows reference it.
var deleteOrder = []string{"host-binding", "pricing", "model", "policy", "rate-limit", "host", "provider"}

// Table lists one kind and deletes a row of it only while the row still has
// the version that was read.
type Table[T any] interface {
	List(ctx context.Context) ([]*T, error)
	DeleteAtVersion(ctx context.Context, id, version string) error
}

// Overlays lists the overlays, whose targets a prune keeps.
type Overlays interface {
	List(ctx context.Context) ([]*overlay.Overlay, error)
}

// Stores are the kinds a release can drop.
type Stores struct {
	Providers  Table[provider.Provider]
	Hosts      Table[host.Host]
	Models     Table[model.Model]
	Pricings   Table[pricing.Pricing]
	Bindings   Table[binding.Binding]
	Policies   Table[policy.Policy]
	RateLimits Table[ratelimit.RateLimit]
	Overlays   Overlays
}

// UsageReader is the one usage read the traffic guard makes.
type UsageReader interface {
	Events(ctx context.Context, q usage.EventQuery) ([]usage.Event, error)
}

// Options configures one prune.
type Options struct {
	// Idle is how long a model or host binding must have served no request.
	Idle time.Duration
	// Apply deletes the rows decided prune; false only decides.
	Apply bool
	// References lists the rows referencing the row id of kind (an API singular).
	References func(ctx context.Context, kind, id string) ([]Ref, error)
	// Usage answers the traffic guard; nil leaves it unknown.
	Usage UsageReader
	// Now defaults to time.Now.
	Now func() time.Time
}

// Report is what a prune decided and, when it applied, did.
type Report struct {
	// Idle is the traffic window the prune judged by.
	Idle time.Duration
	Rows []Decision
	// Pruned counts the rows deleted, or that a dry run would delete.
	Pruned int
	// Kept counts the rows that stay detached.
	Kept    int
	Applied bool
}

// errNoUsageStore is the unknown answer when no usage store is wired.
var errNoUsageStore = errors.New("no usage store is configured")

// Run judges every detached row and, with opts.Apply, deletes the ones
// decided prune. Each delete re-reads the row's references and is
// conditional on the version judged, so a row edited or newly referenced
// meanwhile stays. A failed delete stops the run; rows already deleted stay
// deleted.
func Run(ctx context.Context, s Stores, opts Options) (Report, error) {
	if opts.Idle <= 0 {
		return Report{}, fmt.Errorf("catalogprune: idle window must be positive, got %s", opts.Idle)
	}
	if opts.References == nil {
		return Report{}, errors.New("catalogprune: no reference source")
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	tables := s.tables()
	cands, err := loadCandidates(ctx, tables, s.Overlays)
	if err != nil {
		return Report{}, err
	}
	if err := markNamedModels(ctx, s, cands); err != nil {
		return Report{}, err
	}
	since := now().Add(-opts.Idle)
	unknown, err := gatherFacts(ctx, cands, opts, since)
	if err != nil {
		return Report{}, err
	}
	if unknown != "" {
		slog.Warn("catalog prune: usage history unavailable; detached models and host bindings stay", "reason", unknown)
	}

	rep := Report{Idle: opts.Idle, Rows: decide(cands)}
	if opts.Apply {
		rep.Applied = true
		if err := applyDecisions(ctx, tables, rep.Rows, opts.References); err != nil {
			rep.count()
			return rep, err
		}
	}
	rep.count()
	return rep, nil
}

func (r *Report) count() {
	r.Pruned, r.Kept = 0, 0
	for _, d := range r.Rows {
		if d.Decision == Prune {
			r.Pruned++
		} else {
			r.Kept++
		}
	}
}

// table is one kind's rows as candidates, and its conditional delete.
type table struct {
	kind string
	load func(ctx context.Context) ([]*candidate, error)
	del  func(ctx context.Context, id, version string) error
}

func (s Stores) tables() map[string]table {
	all := []table{
		tableOf("host-binding", s.Bindings, func(b *binding.Binding) *meta.Metadata { return &b.Meta },
			func(b *binding.Binding) *usage.EventQuery {
				return &usage.EventQuery{ModelID: []string{b.Spec.ModelID}, HostID: []string{b.Spec.HostID}}
			}),
		tableOf("pricing", s.Pricings, func(p *pricing.Pricing) *meta.Metadata { return &p.Meta }, nil),
		tableOf("model", s.Models, func(m *model.Model) *meta.Metadata { return &m.Meta },
			func(m *model.Model) *usage.EventQuery { return &usage.EventQuery{ModelID: []string{m.Meta.ID}} }),
		tableOf("policy", s.Policies, func(p *policy.Policy) *meta.Metadata { return &p.Meta }, nil),
		tableOf("rate-limit", s.RateLimits, func(r *ratelimit.RateLimit) *meta.Metadata { return &r.Meta }, nil),
		tableOf("host", s.Hosts, func(h *host.Host) *meta.Metadata { return &h.Meta }, nil),
		tableOf("provider", s.Providers, func(p *provider.Provider) *meta.Metadata { return &p.Meta }, nil),
	}
	out := make(map[string]table, len(all))
	for _, t := range all {
		out[t.kind] = t
	}
	return out
}

// tableOf adapts one kind's store; traffic builds the usage query of a kind
// with a traffic guard and is nil for the others.
func tableOf[T any](kind string, t Table[T], metaOf func(*T) *meta.Metadata, traffic func(*T) *usage.EventQuery) table {
	return table{
		kind: kind,
		load: func(ctx context.Context) ([]*candidate, error) {
			rows, err := t.List(ctx)
			if err != nil {
				return nil, fmt.Errorf("list %s rows: %w", kind, err)
			}
			var out []*candidate
			for _, row := range rows {
				m := metaOf(row)
				if m.CatalogDetachedAt == nil {
					continue
				}
				c := &candidate{kind: kind, id: m.ID, name: m.Name, owner: m.Owner,
					detachedAt: *m.CatalogDetachedAt, version: m.ResourceVersion, dirty: m.Dirty}
				if traffic != nil {
					c.traffic = traffic(row)
				}
				out = append(out, c)
			}
			return out, nil
		},
		del: t.DeleteAtVersion,
	}
}
