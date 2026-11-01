package plan

import (
	"fmt"
	"testing"
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pool"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
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
		w.bindPolicy(p, subject(policybinding.SubjectAccount, a.Meta.ID))
		if i == 0 {
			s.tierPol = p
		}
	}
	for i, h := range hosts {
		w.bindPolicy(w.policy(fmt.Sprintf("host-%d", i), policy.Spec{BudgetIDs: []string{bud.Meta.ID}}), subject(policybinding.SubjectHost, h.Meta.ID))
	}
	principalPolicies := 2000 - len(accounts) - len(hosts) - 100 - 1000
	n := 0
	for ti := range 100 {
		t := w.team()
		tp := w.policy(fmt.Sprintf("team-%d", ti), policy.Spec{
			PoolIDs:    []string{pools[ti%200].Meta.ID, pools[(ti+67)%200].Meta.ID, pools[(ti+133)%200].Meta.ID},
			RateLimits: []policy.RateLimitLink{{RateLimitID: rls[ti%50].Meta.ID}},
			BudgetIDs:  []string{bud.Meta.ID},
		})
		w.bindPolicy(tp, subject(policybinding.SubjectTeam, t.Meta.ID))
		if ti == 0 {
			s.teamPol = tp
		}
		for pi := range 10 {
			p := w.project(t)
			pp := w.policy(fmt.Sprintf("project-%d-%d", ti, pi), policy.Spec{Models: []string{provs[(ti+pi)%5].Meta.Name, provs[(ti+pi+1)%5].Meta.Name}, RateLimits: []policy.RateLimitLink{{RateLimitID: rls[pi].Meta.ID}}})
			w.bindPolicy(pp, subject(policybinding.SubjectProject, p.Meta.ID))
			for range 10 {
				sa := w.sa(p)
				if n < principalPolicies {
					w.bindPolicy(w.policy(fmt.Sprintf("sa-%d", n), policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rls[n%50].Meta.ID}}}), subject(rolebinding.SubjectServiceAccount, sa.Meta.ID))
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
		if len(r.Hops[0].Counters[0]) < 0 {
			b.Fatal()
		}
	}
}
