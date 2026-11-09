package apply

import (
	"context"
	"fmt"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/manifest"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/share"
)

// RevokeImpact is one kind of change revoking a pruned share makes, as the control API's delete reports it.
type RevokeImpact struct {
	Kind   string   `json:"kind"   enum:"policy-binding,key,pool" doc:"Kind of the changed rows, as an API singular."`
	Change string   `json:"change" enum:"drops,deleted,moves" doc:"drops: the rows let go of the share; deleted: policy bindings left with no account go; moves: relay keys whose lane the share was move to their principal's own account."`
	Names  []string `json:"names,omitempty"  doc:"Slugs of the changed rows the caller may see."`
	Hidden int      `json:"hidden,omitempty" doc:"Changed rows the caller may not see."`
}

// manifestKinds maps a revoke's row kinds to manifest kinds, for visibility.
var manifestKinds = map[string]string{"policy-binding": "PolicyBinding", "key": "Key", "pool": "Pool"}

// planShareRevokes runs after accounts are planned. Every pruned share lists what revoking it changes, and a pruned real account names each of its shares as a delete of its own, just before it, so the plan shows them before anyone confirms; the write revokes, never deletes raw. A share the bundle still declares refuses its real account's prune.
func (b *builder) planShareRevokes(ctx context.Context, docs []*manifest.AccountDTO) error {
	pruned := b.deletes[len(b.deletes)-1]
	if len(pruned) == 0 {
		return nil
	}
	declared := make(map[string]bool, len(docs))
	for _, d := range docs {
		declared[d.Metadata.Name] = true
	}
	byID := make(map[string]*account.Account, len(b.rows.Accounts))
	for _, a := range b.rows.Accounts {
		byID[a.Meta.ID] = a
	}
	listed := make(map[string]bool, len(pruned))
	for _, e := range pruned {
		listed[e.ID] = true
	}
	out := make([]Entry, 0, len(pruned))
	for _, e := range pruned {
		for _, s := range b.rows.Accounts {
			if s.Spec.SourceAccountID != e.ID || listed[s.Meta.ID] {
				continue
			}
			if declared[s.Meta.Name] {
				return &InvalidError{Kind: "Account", Name: s.Meta.Name, Err: fmt.Errorf("shares account %q, which this run prunes; drop the share from the bundle too, or keep the account", e.Name)}
			}
			if err := b.governs(settings.OpDelete, "account", &s.Meta); err != nil {
				return &GovernanceError{Kind: "Account", Name: s.Meta.Name, Err: err}
			}
			id, realOwner := s.Meta.ID, e.owner
			out = append(out, Entry{
				Kind: "Account", Name: s.Meta.Name, ID: id, Action: ActionDelete, Revokes: b.revokeImpact(ctx, id),
				plural: "accounts", owner: s.Meta.Owner, authorizeAt: &realOwner,
				prev:  rowState{present: true, dirty: s.Meta.Dirty, updatedAt: s.Meta.UpdatedAt},
				write: func(ctx context.Context) error { return b.deleteAccount(ctx, id) },
			})
			listed[id] = true
		}
		if a := byID[e.ID]; a != nil && a.IsShared() {
			e.Revokes = b.revokeImpact(ctx, e.ID)
		}
		out = append(out, e)
	}
	b.deletes[len(b.deletes)-1] = out
	return nil
}

// revokeImpact groups what revoking share id changes, naming the rows the caller may see.
func (b *builder) revokeImpact(ctx context.Context, id string) []RevokeImpact {
	var out []RevokeImpact
	at := map[[2]string]int{}
	for _, c := range share.Impact(share.Rows{Bindings: b.rows.PolicyBindings, Keys: b.rows.Keys, Pools: b.rows.Pools}, id) {
		change := "drops"
		switch {
		case c.Deleted:
			change = "deleted"
		case c.Kind == "key":
			change = "moves"
		}
		k := [2]string{c.Kind, change}
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, RevokeImpact{Kind: c.Kind, Change: change})
		}
		if b.visible(ctx, manifestKinds[c.Kind], c.ID, c.Owner) {
			out[i].Names = append(out[i].Names, c.Name)
		} else {
			out[i].Hidden++
		}
	}
	return out
}

// deleteAccount revokes a share, so its bindings, keys and pools are changed as the control API's delete changes them, and deletes any other account.
func (b *builder) deleteAccount(ctx context.Context, id string) error {
	for _, a := range b.rows.Accounts {
		if a.Meta.ID == id && a.IsShared() {
			_, err := share.Revoke(ctx, b.shareStores(), id)
			return err
		}
	}
	return b.opts.Stores.Account.Delete(ctx, id)
}

func (b *builder) shareStores() share.Stores {
	s := b.opts.Stores
	return share.Stores{Account: s.Account, PolicyBinding: s.PolicyBinding, Key: s.Key, Pool: s.Pool, OwnAccount: b.ownAccount}
}

// ownAccount is a principal's own account, made on first use: where a relay key goes when the share it ran in is revoked.
func (b *builder) ownAccount(ctx context.Context, p key.Principal) (string, error) {
	s := b.opts.Stores
	switch p.Kind {
	case key.PrincipalUser:
		u, err := s.User.Get(ctx, p.ID)
		if err != nil || u == nil {
			return "", fmt.Errorf("user %q: %w", p.ID, err)
		}
		return s.Account.EnsureManaged(ctx, account.Personal(u.ID, u.Username))
	case key.PrincipalServiceAccount:
		sa, err := s.ServiceAccount.Get(ctx, p.ID)
		if err != nil || sa == nil {
			return "", fmt.Errorf("service account %q: %w", p.ID, err)
		}
		return s.Account.EnsureManaged(ctx, account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, sa.Meta.DisplayName, sa.Spec.ProjectID))
	}
	return "", fmt.Errorf("principal kind %q has no account of its own", p.Kind)
}
