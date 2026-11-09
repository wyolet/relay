package control

import (
	"context"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/refcheck"
)

// principalNamer names users (by username) and service accounts (by display name) for the holder and principal fields of rows a read returns. Seeing a row means seeing who holds it, so a caller who may not list users still reads the names on rows they see. A list loads both kinds once; a single row reads only its own.
type principalNamer struct {
	d          Deps
	users, sas map[string]string
}

// newPrincipalNamer returns a namer for one read; loadAll reads every user and service account up front, for a list.
func newPrincipalNamer(ctx context.Context, d Deps, loadAll bool) *principalNamer {
	n := &principalNamer{d: d}
	if !loadAll {
		return n
	}
	n.users, n.sas = map[string]string{}, map[string]string{}
	if d.Users != nil {
		if all, err := d.Users.List(ctx); err == nil {
			for _, u := range all {
				n.users[u.ID] = u.Username
			}
		}
	}
	if d.Stores != nil && d.Stores.ServiceAccount != nil {
		if all, err := d.Stores.ServiceAccount.List(ctx); err == nil {
			for _, sa := range all {
				n.sas[sa.Meta.ID] = label(sa.Meta)
			}
		}
	}
	return n
}

func (n *principalNamer) user(ctx context.Context, id string) string {
	if n.users != nil {
		return n.users[id]
	}
	if n.d.Users != nil {
		if u, err := n.d.Users.Get(ctx, id); err == nil && u != nil {
			return u.Username
		}
	}
	return ""
}

func (n *principalNamer) serviceAccount(ctx context.Context, id string) string {
	if n.sas != nil {
		return n.sas[id]
	}
	if n.d.Stores != nil && n.d.Stores.ServiceAccount != nil {
		if sa, err := n.d.Stores.ServiceAccount.Get(ctx, id); err == nil && sa != nil {
			return label(sa.Meta)
		}
	}
	return ""
}

// nameHolder fills a's holder name when az shows the caller a.
func (n *principalNamer) nameHolder(ctx context.Context, az authz.Authorizer, a *account.Account) {
	h := a.Spec.Holder
	if h == nil || !refcheck.Visible(ctx, az, "account", a.Meta.ID, a.Meta.Owner) {
		return
	}
	switch h.Kind {
	case account.HolderUser:
		h.Name = n.user(ctx, h.ID)
	case account.HolderServiceAccount:
		h.Name = n.serviceAccount(ctx, h.ID)
	}
}

// namePrincipal fills k's principal name when az shows the caller k.
func (n *principalNamer) namePrincipal(ctx context.Context, az authz.Authorizer, k *key.Key) {
	if !refcheck.Visible(ctx, az, "key", k.Meta.ID, k.Meta.Owner) {
		return
	}
	p := &k.Spec.Principal
	switch p.Kind {
	case key.PrincipalUser:
		p.Name = n.user(ctx, p.ID)
	case key.PrincipalServiceAccount:
		p.Name = n.serviceAccount(ctx, p.ID)
	}
}

// enrichKey fills one key's principal name.
func enrichKey(d Deps) enrichFn[key.Key] {
	return func(ctx context.Context, k *key.Key) {
		newPrincipalNamer(ctx, d, false).namePrincipal(ctx, d.Authz, k)
	}
}

// enrichKeys fills the principal names of a key list, reading users and service accounts once.
func enrichKeys(d Deps) enrichListFn[key.Key] {
	return func(ctx context.Context, keys []*key.Key) {
		n := newPrincipalNamer(ctx, d, len(keys) > 1)
		for _, k := range keys {
			n.namePrincipal(ctx, d.Authz, k)
		}
	}
}
