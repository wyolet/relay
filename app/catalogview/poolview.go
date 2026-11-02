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
	ID     string `json:"id"     doc:"Pool or account id."`
	Name   string `json:"name"   doc:"Pool or account slug."`
	HostID string `json:"hostId" doc:"Id of the host its keys authenticate to."`
}

// PoolKeys returns the pool's keys in pool order, the preference a
// prioritized pool tries them in: each listed account's keys in creation
// order, then the listed keys, a key reached twice at its first place. A
// shared account's keys are its real account's, listed read-only; other
// keys the caller may not see are dropped.
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
	placed := map[string]bool{}
	add := func(k *hostkey.HostKey, readOnly bool) {
		if placed[k.Meta.ID] || (!readOnly && !s.visible("host-key", k.Meta.ID, k.Meta.Owner)) {
			return
		}
		placed[k.Meta.ID] = true
		v := hostKeyView(k)
		v.ReadOnly = readOnly
		rows = append(rows, v)
	}
	if len(p.Spec.AccountIDs) > 0 {
		accounts, err := s.accountsByID(ctx)
		if err != nil {
			return UpstreamRef{}, nil, err
		}
		for _, aid := range p.Spec.AccountIDs {
			a := accounts[aid]
			if a == nil {
				continue
			}
			for _, k := range keysOf(keys, a) {
				add(k, a.IsShared())
			}
		}
	}
	for _, id := range p.Spec.CredentialIDs {
		if k, ok := byID[id]; ok {
			add(k, false)
		}
	}
	return UpstreamRef{ID: p.Meta.ID, Name: p.Meta.Name, HostID: p.Spec.HostID}, rows, nil
}

func (s *Service) accountsByID(ctx context.Context) (map[string]*account.Account, error) {
	out := map[string]*account.Account{}
	if s.Accounts == nil {
		return out, nil
	}
	all, err := s.Accounts.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range all {
		out[a.Meta.ID] = a
	}
	return out, nil
}

// keysOf returns the keys a holds in creation order: a shared account's are its real account's.
func keysOf(keys []*hostkey.HostKey, a *account.Account) []*hostkey.HostKey {
	holder := a.Meta.ID
	if a.IsShared() {
		holder = a.Spec.SourceAccountID
	}
	var out []*hostkey.HostKey
	for _, k := range keys {
		if k.Spec.AccountID == holder {
			out = append(out, k)
		}
	}
	// Ids are UUIDv7, so id order is creation order.
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.ID < out[j].Meta.ID })
	return out
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
	for _, k := range keysOf(keys, a) {
		// Whoever sees a shared account sees its real account's keys, read-only.
		if a.IsShared() || s.visible("host-key", k.Meta.ID, k.Meta.Owner) {
			v := hostKeyView(k)
			v.ReadOnly = a.IsShared()
			rows = append(rows, v)
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
		Prefix:      k.Prefix(),
	}
}
