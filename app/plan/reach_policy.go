package plan

import (
	"slices"
	"strings"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/engine"
)

// PolicyReach reads saved policy p where it applies. Bound, it is read in each account its enabled bindings name, once per distinct ladder, and merged: a model any of them reaches is reached, with the accounts that reach it; one none reaches is excluded, with one ladder's refusal and the accounts it holds for. Unbound, it is read in its owner's own account as a draft is. nil when neither can be read.
func PolicyReach(v *engine.View, p *policy.Policy) *Reach {
	type ladder struct {
		accounts []*account.Account
	}
	var order []ladderKey
	ladders := map[ladderKey]*ladder{}
	for b := range engine.All[policybinding.PolicyBinding](v) {
		if b.Spec.PolicyID != p.Meta.ID || !b.IsEnabled() {
			continue
		}
		for _, id := range b.Spec.AccountIDs {
			acc := engine.Get[account.Account](v, id)
			if acc == nil {
				continue
			}
			k := ladderKeyOf(v, acc)
			if ladders[k] == nil {
				ladders[k] = &ladder{}
				order = append(order, k)
			}
			ladders[k].accounts = append(ladders[k].accounts, acc)
		}
	}
	if len(order) == 0 {
		acc, ok := OwnerAccount(v, p.Meta.Owner)
		if !ok {
			return nil
		}
		over := &Override{Policy: p}
		if acc != nil {
			over.AccountID = acc.Meta.ID
		}
		return ReachOf(v, over.AccountID, over)
	}
	m := reachMerge{byModel: map[string]int{}, exByModel: map[string]int{}}
	for _, k := range order {
		l := ladders[k]
		rep := l.accounts[0]
		cp := foldFor(v, rep, nil)
		if cp == nil {
			continue
		}
		var dropped []DroppedLink
		if rp := engine.Get[RungPolicy](v, accountSubject(rep.Meta.ID)); rp != nil && rp.Rung.Policy != nil && rp.Rung.Policy.Policy.Meta.ID == p.Meta.ID {
			dropped = droppedLinks(v, cp, rp.Rung.Policy, rep.Meta.Owner, rp.unusable)
		}
		m.add(walkReach(cp, rep), l.accounts, dropped)
	}
	return m.reach()
}

// reachMerge folds the reads of several ladders into one answer.
type reachMerge struct {
	refused            *Refusal
	readable, refusing int
	models, excluded   []ModelReach
	byModel, exByModel map[string]int
	dropped            []DroppedLink
	droppedIn          map[droppedKey]int
}

type droppedKey struct{ pool, account, credential, code string }

func keyOfDropped(d DroppedLink) droppedKey {
	k := droppedKey{pool: d.PoolID, code: d.Code}
	if d.Account != nil {
		k.account = d.Account.Meta.ID
	}
	if d.Credential != nil {
		k.credential = d.Credential.Meta.ID
	}
	return k
}

func (m *reachMerge) add(r *Reach, accounts []*account.Account, dropped []DroppedLink) {
	m.readable++
	if r.Refused != nil {
		m.refusing++
		if m.refused == nil {
			m.refused = r.Refused
		}
		return
	}
	for _, mr := range r.Models {
		i, ok := m.byModel[mr.Model.Meta.ID]
		if !ok {
			mr.Accounts = nil
			m.byModel[mr.Model.Meta.ID] = len(m.models)
			m.models = append(m.models, mr)
			i = len(m.models) - 1
		} else {
			mergeModel(&m.models[i], mr)
		}
		m.models[i].Accounts = append(m.models[i].Accounts, accounts...)
	}
	for _, mr := range r.Excluded {
		i, ok := m.exByModel[mr.Model.Meta.ID]
		if !ok {
			mr.Accounts = nil
			m.exByModel[mr.Model.Meta.ID] = len(m.excluded)
			m.excluded = append(m.excluded, mr)
			i = len(m.excluded) - 1
		}
		m.excluded[i].Accounts = append(m.excluded[i].Accounts, accounts...)
	}
	// A dropped link counts only when every ladder read drops it.
	if m.droppedIn == nil {
		m.droppedIn = map[droppedKey]int{}
	}
	for _, d := range dropped {
		k := keyOfDropped(d)
		if m.droppedIn[k] == 0 {
			m.dropped = append(m.dropped, d)
		}
		m.droppedIn[k]++
	}
}

func (m *reachMerge) reach() *Reach {
	out := &Reach{}
	if m.readable > 0 && m.refusing == m.readable {
		out.Refused = m.refused
		return out
	}
	out.Models = m.models
	for _, mr := range m.excluded {
		if _, reached := m.byModel[mr.Model.Meta.ID]; !reached {
			out.Excluded = append(out.Excluded, mr)
		}
	}
	for _, d := range m.dropped {
		if m.droppedIn[keyOfDropped(d)] == m.readable-m.refusing {
			out.Dropped = append(out.Dropped, d)
		}
	}
	sortByName(out.Models)
	sortByName(out.Excluded)
	return out
}

// mergeModel adds to dst the hosts, ways and upstream verdicts src has that it lacks.
func mergeModel(dst *ModelReach, src ModelReach) {
	for _, h := range src.Hosts {
		if !containsHost(dst.Hosts, h) {
			dst.Hosts = append(dst.Hosts, h)
		}
	}
	for _, v := range src.Via {
		if !containsVia(dst.Via, v) {
			dst.Via = append(dst.Via, v)
		}
	}
	for _, u := range src.Upstream {
		dup := false
		for _, x := range dst.Upstream {
			if x.Account == u.Account && x.SharedAs == u.SharedAs {
				dup = true
				break
			}
		}
		if !dup {
			dst.Upstream = append(dst.Upstream, u)
		}
	}
}

func sortByName(ms []ModelReach) {
	slices.SortStableFunc(ms, func(a, b ModelReach) int { return strings.Compare(a.Model.Meta.Name, b.Model.Meta.Name) })
}
