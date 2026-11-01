package catalogview

import (
	"context"
	"sort"

	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
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
	ID          string `json:"id"                    doc:"Host key id."`
	Name        string `json:"name"                  doc:"Host key slug."`
	Kind        string `json:"kind"                  enum:"env,stored,oauth" doc:"Where the secret comes from; the secret itself is never shown."` // "env" | "stored"
	DefaultTier string `json:"defaultTier,omitempty" deprecated:"true" doc:"Stored but not applied to requests."`
	Enabled     bool   `json:"enabled"               doc:"Whether the key is enabled."`
}

// HostPolicyRow — a host-tier policy this host owns, with its rate-limit rule
// sets (model-scoped and unscoped links alike).
type HostPolicyRow struct {
	ID         string               `json:"id"         doc:"Policy id."`
	Name       string               `json:"name"       doc:"Policy slug."`
	Enabled    bool                 `json:"enabled"    doc:"Whether the policy is enabled."`
	RateLimits []PolicyRateLimitRow `json:"rateLimits" doc:"The rate limits the preset links, model-scoped and unscoped."`
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
		if !s.visible("host-key", k.Meta.ID, k.Meta.Owner) {
			continue
		}
		rows = append(rows, hostKeyView(k))
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
