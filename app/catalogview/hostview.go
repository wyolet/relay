package catalogview

import (
	"context"
	"sort"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policy"
)

// hostview.go is the host-detail mirror of the model/policy projections. A Host
// owns the things its detail page navigates to:
//
//   - models   — the models it serves, via bindings (see HostModels).
//   - keys     — the upstream credentials it owns (HostKey.owner targets it).
//                Value mode (env/stored) is shown; the secret never is.
//   - policies — the host-tier serving policies it owns (Policy.owner=host),
//                each with the rate-limit rule sets it applies.

// HostKeyView — one upstream credential targeting this host. Secret-free:
// only the value-mode discriminator and non-sensitive config.
type HostKeyView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"` // "env" | "stored"
	DefaultTier string `json:"defaultTier,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// HostPolicyRow — a host-tier policy this host owns, with its rate-limit rule
// sets (per-model RLBindings + the flat default).
type HostPolicyRow struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Enabled    bool                 `json:"enabled"`
	RateLimits []PolicyRateLimitRow `json:"rateLimits"`
}

// loadHost resolves the host {ref} (id or slug) and builds the index.
func (s *Service) loadHost(ctx context.Context, ref string) (*host.Host, *index, error) {
	idx, err := s.buildIndex(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, h := range idx.hostByID {
		if h.Meta.ID == ref || h.Meta.Name == ref {
			return h, idx, nil
		}
	}
	return nil, nil, ErrNotFound
}

// HostKeyList returns the upstream credentials this host owns (by id or slug).
func (s *Service) HostKeyList(ctx context.Context, ref string) (HostRef, []HostKeyView, error) {
	h, idx, err := s.loadHost(ctx, ref)
	if err != nil {
		return HostRef{}, nil, err
	}
	rows := []HostKeyView{}
	for _, k := range idx.hostkeyByID {
		if k.Spec.HostID != h.Meta.ID {
			continue
		}
		rows = append(rows, HostKeyView{
			ID:          k.Meta.ID,
			Name:        k.Meta.Name,
			Kind:        string(k.Spec.ValueFrom.Kind),
			DefaultTier: k.Spec.DefaultTier,
			Enabled:     hostKeyEnabled(k),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return hostRefOf(h), rows, nil
}

// HostPolicies returns the host-tier policies this host owns, with limits.
func (s *Service) HostPolicies(ctx context.Context, ref string) (HostRef, []HostPolicyRow, error) {
	h, idx, err := s.loadHost(ctx, ref)
	if err != nil {
		return HostRef{}, nil, err
	}
	rows := []HostPolicyRow{}
	for _, p := range idx.policies {
		if p.Meta.Owner.Kind != meta.OwnerHost || p.Meta.Owner.ID != h.Meta.ID {
			continue
		}
		rows = append(rows, HostPolicyRow{
			ID:         p.Meta.ID,
			Name:       p.Meta.Name,
			Enabled:    p.IsEnabled(),
			RateLimits: idx.policyRateLimitRows(p),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return hostRefOf(h), rows, nil
}

func hostKeyEnabled(k *hostkey.HostKey) bool {
	return k.Spec.Enabled == nil || *k.Spec.Enabled
}

// policyRateLimitRows is shared by PolicyRateLimits and HostPolicies: each
// per-model RLBinding in declared order, then the flat default last.
func (idx *index) policyRateLimitRows(p *policy.Policy) []PolicyRateLimitRow {
	rows := []PolicyRateLimitRow{}
	for _, b := range p.Spec.RLBindings {
		rows = append(rows, idx.rlRow(b.RateLimitID, b.Models, false))
	}
	if p.Spec.RateLimitID != "" {
		rows = append(rows, idx.rlRow(p.Spec.RateLimitID, nil, true))
	}
	return rows
}
