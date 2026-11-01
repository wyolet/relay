package manifest

import (
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pool"
)

// ---------------------------------------------------------------------------
// Account / Pool / Budget
// ---------------------------------------------------------------------------

// ToAccount resolves the host name.
func ToAccount(d AccountDTO, idx Resolver) (*account.Account, error) {
	hostID, err := hostOf(idx, "account", d.Metadata.Name, d.Spec.Host)
	if err != nil {
		return nil, err
	}
	m := d.Metadata.toMeta()
	resolveScopeOwner(&m.Owner, idx)
	return &account.Account{
		Meta: m,
		Spec: account.Spec{HostID: hostID, OrgID: d.Spec.OrgID, Default: d.Spec.Default},
	}, nil
}

func FromAccount(a *account.Account, rev ReverseResolver) AccountDTO {
	return AccountDTO{
		APIVersion: APIVersion,
		Kind:       "Account",
		Metadata:   metaToWire(a.Meta),
		Spec:       AccountSpec{Host: nameOr(rev.HostName, a.Spec.HostID), OrgID: a.Spec.OrgID, Default: a.Spec.Default},
	}
}

// ToPool resolves the host and every credential by name and fills the
// default selection, so the stored row and its manifest converge.
func ToPool(d PoolDTO, idx Resolver) (*pool.Pool, error) {
	hostID, err := hostOf(idx, "pool", d.Metadata.Name, d.Spec.Host)
	if err != nil {
		return nil, err
	}
	creds, err := resolveNames(idx.HostKeyID, d.Spec.Credentials, "pool %q: credential %q not found", d.Metadata.Name)
	if err != nil {
		return nil, err
	}
	m := d.Metadata.toMeta()
	resolveScopeOwner(&m.Owner, idx)
	p := &pool.Pool{
		Meta: m,
		Spec: pool.Spec{
			HostID:        hostID,
			CredentialIDs: creds,
			Selection:     keypool.KeySelection(d.Spec.Selection),
		},
	}
	p.Default()
	return p, nil
}

func FromPool(p *pool.Pool, rev ReverseResolver) PoolDTO {
	return PoolDTO{
		APIVersion: APIVersion,
		Kind:       "Pool",
		Metadata:   metaToWire(p.Meta),
		Spec: PoolSpec{
			Host:        nameOr(rev.HostName, p.Spec.HostID),
			Credentials: namesOf(rev.HostKeyName, p.Spec.CredentialIDs),
			Selection:   string(p.Spec.Selection),
		},
	}
}

// ToBudget fills the default period and mode, so the stored row and its
// manifest converge.
func ToBudget(d BudgetDTO, idx Resolver) (*budget.Budget, error) {
	m := d.Metadata.toMeta()
	resolveScopeOwner(&m.Owner, idx)
	b := &budget.Budget{
		Meta: m,
		Spec: budget.Spec{
			AmountUSD: d.Spec.AmountUSD,
			Period:    budget.Period(d.Spec.Period),
			Mode:      budget.Mode(d.Spec.Mode),
		},
	}
	b.Default()
	return b, nil
}

func FromBudget(b *budget.Budget, _ ReverseResolver) BudgetDTO {
	return BudgetDTO{
		APIVersion: APIVersion,
		Kind:       "Budget",
		Metadata:   metaToWire(b.Meta),
		Spec: BudgetSpec{
			AmountUSD: b.Spec.AmountUSD,
			Period:    string(b.Spec.Period),
			Mode:      string(b.Spec.Mode),
		},
	}
}

// hostOf resolves the host a kind's spec.host names; empty is left for the
// row's Validate to report.
func hostOf(idx Resolver, kind, name, host string) (string, error) {
	if host == "" {
		return "", nil
	}
	id, ok := idx.HostID(host)
	if !ok {
		return "", refNotFound("%s %q: host %q not found", kind, name, host)
	}
	return id, nil
}
