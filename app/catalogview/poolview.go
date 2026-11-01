package catalogview

import (
	"context"
	"sort"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pool"
)

// accountStore is the read an account projection needs.
type accountStore interface {
	List(context.Context) ([]*account.Account, error)
}

// UpstreamRef names the pool or account a key list belongs to.
type UpstreamRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	HostID string `json:"hostId"`
}

// PoolKeys returns the pool's keys in pool order, the preference a
// prioritized pool tries them in. Keys the caller may not see are dropped.
func (s *Service) PoolKeys(ctx context.Context, ref string) (UpstreamRef, []HostKeyView, error) {
	if s.Pools == nil {
		return UpstreamRef{}, nil, ErrNotFound
	}
	pools, err := s.Pools.List(ctx)
	if err != nil {
		return UpstreamRef{}, nil, err
	}
	var p *pool.Pool
	for _, x := range pools {
		if x.Meta.ID == ref || x.Meta.Name == ref {
			p = x
			break
		}
	}
	if p == nil || !s.visible("pool", p.Meta.ID, p.Meta.Owner) {
		return UpstreamRef{}, nil, ErrNotFound
	}
	keys, err := s.HostKeys.List(ctx)
	if err != nil {
		return UpstreamRef{}, nil, err
	}
	byID := make(map[string]*hostkey.HostKey, len(keys))
	for _, k := range keys {
		byID[k.Meta.ID] = k
	}
	rows := []HostKeyView{}
	for _, id := range p.Spec.CredentialIDs {
		if k, ok := byID[id]; ok && s.visible("host-key", k.Meta.ID, k.Meta.Owner) {
			rows = append(rows, hostKeyView(k))
		}
	}
	return UpstreamRef{ID: p.Meta.ID, Name: p.Meta.Name, HostID: p.Spec.HostID}, rows, nil
}

// AccountKeys returns the host keys that belong to the account: the keys
// sharing its upstream tier limits.
func (s *Service) AccountKeys(ctx context.Context, ref string) (UpstreamRef, []HostKeyView, error) {
	if s.Accounts == nil {
		return UpstreamRef{}, nil, ErrNotFound
	}
	accounts, err := s.Accounts.List(ctx)
	if err != nil {
		return UpstreamRef{}, nil, err
	}
	var a *account.Account
	for _, x := range accounts {
		if x.Meta.ID == ref || x.Meta.Name == ref {
			a = x
			break
		}
	}
	if a == nil || !s.visible("account", a.Meta.ID, a.Meta.Owner) {
		return UpstreamRef{}, nil, ErrNotFound
	}
	keys, err := s.HostKeys.List(ctx)
	if err != nil {
		return UpstreamRef{}, nil, err
	}
	rows := []HostKeyView{}
	for _, k := range keys {
		if k.Spec.AccountID == a.Meta.ID && s.visible("host-key", k.Meta.ID, k.Meta.Owner) {
			rows = append(rows, hostKeyView(k))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return UpstreamRef{ID: a.Meta.ID, Name: a.Meta.Name, HostID: a.Spec.HostID}, rows, nil
}

func hostKeyView(k *hostkey.HostKey) HostKeyView {
	return HostKeyView{
		ID:          k.Meta.ID,
		Name:        k.Meta.Name,
		Kind:        string(k.Spec.ValueFrom.Kind),
		DefaultTier: k.Spec.DefaultTier,
		Enabled:     hostKeyEnabled(k),
	}
}
