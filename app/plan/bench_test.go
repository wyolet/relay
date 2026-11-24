package plan

import (
	"fmt"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/meta"
)

// synthetic is a deployment at the sizes the plans are designed for: 50k keys, 10k principals, 2k policies, 200 pools, 1k credentials, 100 accounts, over 500 models on 20 hosts.
type synthetic struct {
	w       *world
	teamPol *policy.Policy
	tierPol *policy.Policy
	model   *model.Model
	key     *key.Key
}

func newSynthetic() *synthetic {
	w := &world{}
	s := &synthetic{w: w}
	var provs []*provider.Provider
	for i := range 5 {
		provs = append(provs, w.provider(fmt.Sprintf("prov-%d", i)))
	}
	var hosts []*host.Host
	for i := range 20 {
		hosts = append(hosts, w.host(fmt.Sprintf("host-%d", i)))
	}
	for i := range 500 {
		m := w.model(provs[i%5], fmt.Sprintf("model-%d", i))
		for j := range 2 {
			h := hosts[(i+j*7)%20]
			w.bind(m, h)
			if i%10 != 0 {
				w.price(m, h)
			}
		}
	}
	s.model = w.models[0]
	var rls []*ratelimit.RateLimit
	for i := range 50 {
		rls = append(rls, w.rateLimit(rule(ratelimit.MeterRequests, int64(100+i), time.Minute), rule(ratelimit.MeterTokens, int64(10000+i), time.Minute)))
	}
	credsByHost := map[string][]*hostkey.HostKey{}
	var accounts []*account.Account
	for i := range 100 {
		h := hosts[i%20]
		a := w.account(h)
		accounts = append(accounts, a)
		for range 10 {
			credsByHost[h.Meta.ID] = append(credsByHost[h.Meta.ID], w.cred(h, a, system))
		}
	}
	var pools []*pool.Pool
	for i := range 200 {
		h := hosts[i%20]
		cs := credsByHost[h.Meta.ID]
		off := (i / 20) * 5 % len(cs)
		pools = append(pools, w.pool(h, cs[off], cs[(off+1)%len(cs)], cs[(off+2)%len(cs)], cs[(off+3)%len(cs)], cs[(off+4)%len(cs)]))
	}
	bud := w.budget("1000")
	// 100 account tier policies with a rule scoped to one provider's models, 20 host policies.
	for i, a := range accounts {
		p := w.policy(fmt.Sprintf("tier-%d", i), policy.Spec{RateLimits: []policy.RateLimitLink{
			{RateLimitID: rls[i%50].Meta.ID},
			{RateLimitID: rls[(i+1)%50].Meta.ID, Models: []string{provs[i%5].Meta.Name}},
		}})
		w.bindPolicy(p, a)
		if i == 0 {
			s.tierPol = p
		}
	}
	principalPolicies := 2000 - len(accounts) - 100 - 1000
	n := 0
	for ti := range 100 {
		t := w.team()
		tp := w.policy(fmt.Sprintf("team-%d", ti), policy.Spec{
			PoolIDs:    []string{pools[ti%200].Meta.ID, pools[(ti+67)%200].Meta.ID, pools[(ti+133)%200].Meta.ID},
			RateLimits: []policy.RateLimitLink{{RateLimitID: rls[ti%50].Meta.ID}},
			Budgets:    []policy.BudgetLink{{BudgetID: bud.Meta.ID}},
		})
		w.bindPolicy(tp, w.teamAccount(t))
		if ti == 0 {
			s.teamPol = tp
		}
		for pi := range 10 {
			p := w.project(t)
			pp := w.policy(fmt.Sprintf("project-%d-%d", ti, pi), policy.Spec{Models: []string{provs[(ti+pi)%5].Meta.Name, provs[(ti+pi+1)%5].Meta.Name}, RateLimits: []policy.RateLimitLink{{RateLimitID: rls[pi].Meta.ID}}})
			w.bindPolicy(pp, w.projectAccount(p))
			for range 10 {
				sa := w.sa(p)
				if n < principalPolicies {
					w.bindPolicy(w.policy(fmt.Sprintf("sa-%d", n), policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rls[n%50].Meta.ID}}}), w.saAccount(sa))
				}
				n++
				for range 5 {
					k := w.saKey(sa)
					if s.key == nil {
						s.key = k
					}
				}
			}
		}
	}
	return s
}

// BenchmarkPlans measures a full load and the rebuild after one write of each kind. Run with a short fixed -benchtime (e.g. 5x): a load builds every plan.
func BenchmarkPlans(b *testing.B) {
	s := newSynthetic()
	b.Logf("rows: keys=%d principals=%d policies=%d pools=%d credentials=%d accounts=%d models=%d", len(s.w.keys), len(s.w.sas), len(s.w.policies), len(s.w.pools), len(s.w.creds), len(s.w.accounts), len(s.w.models))
	b.Run("load", func(b *testing.B) {
		for b.Loop() {
			s.w.start(b)
		}
	})
	h := s.w.start(b)
	b.Run("key-write", func(b *testing.B) {
		alt := [2]*key.Key{clone(s.key), clone(s.key)}
		alt[1].Spec.PassthroughAllowed = true
		i := 0
		for b.Loop() {
			i++
			apply(h, alt[i%2])
		}
	})
	b.Run("team-policy-write", func(b *testing.B) {
		alt := [2]*policy.Policy{clone(s.teamPol), clone(s.teamPol)}
		alt[1].Spec.Models = []string{"prov-0"}
		i := 0
		for b.Loop() {
			i++
			apply(h, alt[i%2])
		}
	})
	b.Run("account-tier-write", func(b *testing.B) {
		alt := [2]*policy.Policy{clone(s.tierPol), clone(s.tierPol)}
		alt[1].Spec.Models = []string{"prov-0", "prov-1"}
		i := 0
		for b.Loop() {
			i++
			apply(h, alt[i%2])
		}
	})
	b.Run("catalog-write", func(b *testing.B) {
		alt := [2]*model.Model{clone(s.model), clone(s.model)}
		alt[1].Meta.DisplayName = "renamed"
		i := 0
		for b.Loop() {
			i++
			apply(h, alt[i%2])
		}
	})
}

// BenchmarkCheck measures one plan check of an agent's rows (service account, lane, pool, policy, binding, key) over 2k models on 4k bindings, 500 credentials, 300 service accounts with a key each. Run with a short fixed -benchtime: each check builds every plan.
func BenchmarkCheck(b *testing.B) {
	w := &world{}
	var provs []*provider.Provider
	for i := range 5 {
		provs = append(provs, w.provider(fmt.Sprintf("prov-%d", i)))
	}
	var hosts []*host.Host
	for i := range 20 {
		hosts = append(hosts, w.host(fmt.Sprintf("host-%d", i)))
	}
	for i := range 2000 {
		m := w.model(provs[i%5], fmt.Sprintf("model-%d", i))
		for j := range 2 {
			h := hosts[(i+j*7)%20]
			w.bind(m, h)
			w.price(m, h)
		}
	}
	var accounts []*account.Account
	for i := range 100 {
		a := w.account(hosts[i%20])
		accounts = append(accounts, a)
		for range 5 {
			w.cred(hosts[i%20], a, system)
		}
	}
	var pools []*pool.Pool
	for i := range 100 {
		p := w.pool(hosts[i%20])
		p.Spec.AccountIDs = []string{accounts[i].Meta.ID}
		pools = append(pools, p)
	}
	var proj *project.Project
	for ti := range 20 {
		t := w.team()
		w.bindPolicy(w.policy(fmt.Sprintf("team-%d", ti), policy.Spec{PoolIDs: []string{pools[ti%100].Meta.ID, pools[(ti+50)%100].Meta.ID}}), w.teamAccount(t))
		for range 5 {
			proj = w.project(t)
			for range 3 {
				w.saKey(w.sa(proj))
			}
		}
	}
	h := w.start(b)
	b.Logf("rows: models=%d bindings=%d credentials=%d service accounts=%d keys=%d", len(w.models), len(w.bindings), len(w.creds), len(w.sas), len(w.keys))

	scope := meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}
	sa := &serviceaccount.ServiceAccount{Meta: md("agent-"+seq(), scope), Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID}}
	lane := account.ForServiceAccount(sa.Meta.ID, sa.Meta.Name, "", scope)
	lane.Meta.ID = meta.NewID()
	agentPool := &pool.Pool{Meta: md("agent-pool-"+seq(), scope), Spec: pool.Spec{HostID: hosts[0].Meta.ID, AccountIDs: []string{accounts[0].Meta.ID}}}
	pol := &policy.Policy{Meta: md("agent-policy-"+seq(), scope), Spec: policy.Spec{PoolIDs: []string{agentPool.Meta.ID}, Models: []string{"prov-0"}}}
	pb := &policybinding.PolicyBinding{Meta: md("agent-pb-"+seq(), scope), Spec: policybinding.Spec{PolicyID: pol.Meta.ID, AccountIDs: []string{lane.Meta.ID}}}
	k := &key.Key{Meta: md("agent-key-"+seq(), scope), Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}, AccountID: lane.Meta.ID, KeyHash: keyHash("agent")}}
	rows := []any{sa, lane, agentPool, pol, pb, k}

	c := NewCheck(false, time.Minute)
	for b.Loop() {
		if _, err := c.NewIssues(b.Context(), h.e.View(), nil, rows); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkChangedReach measures the comparison of every caller account's reach before and after one team's policy narrows, over the synthetic deployment's ten thousand service account lanes, thousand projects and hundred teams. Run with a short fixed -benchtime.
func BenchmarkChangedReach(b *testing.B) {
	s := newSynthetic()
	h := s.w.start(b)
	before := h.e.View()
	narrowed := clone(s.teamPol)
	narrowed.Spec.Models = []string{"prov-0"}
	apply(h, narrowed)
	after := h.e.View()
	b.Logf("accounts=%d", len(s.w.accounts)+len(s.w.sas))
	none := func(*CallerPlan) bool { return false }
	for b.Loop() {
		if changed, err := ChangedReach(b.Context(), before, after, none); err != nil || len(changed) == 0 {
			b.Fatalf("changed %d, %v; want some", len(changed), err)
		}
	}
}

// BenchmarkRequestPath is the read side a request walks once the route is filled: key lookup, route hit, the first hop's counters.
func BenchmarkRequestPath(b *testing.B) {
	bs := newBasic()
	h := bs.w.start(b)
	hash, now := bs.key.Spec.KeyHash, time.Now()
	h.route(bs.key, "m1")
	b.ReportAllocs()
	for b.Loop() {
		kp := h.holder.Load().Keys.Lookup(hash, now)
		r := kp.Caller.Routes.Get("m1")
		if r.Refused != nil {
			b.Fatal(r.Refused)
		}
	}
}
