package pipeline

import (
	"time"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/pool"
	appratelimit "github.com/wyolet/relay/app/ratelimit"
)

var upstreamHost = &host.Host{Meta: meta.Metadata{ID: "host-1", Name: "upstream"}}

// callerCounter is every rule of rl as a counter of the project rung, grouped under team t.
func callerCounter(rl *appratelimit.RateLimit) []limits.Counter {
	var out []limits.Counter
	for _, r := range rl.Spec.Rules {
		out = append(out, limits.CallerRule(limits.RungProject, rl, r, "pol-caller", "project:p", "team:t"))
	}
	return out
}

// accountCounters is every rule of rl as a counter of account id.
func accountCounters(id string, rl *appratelimit.RateLimit) []limits.Counter {
	var out []limits.Counter
	for _, r := range rl.Spec.Rules {
		out = append(out, limits.UpstreamRule(limits.RungAccount, rl, r, "pol-tier", "account:"+id, "", "account:"+id))
	}
	return out
}

// testSlot is one credential at upstreamHost in acc (nil for none).
type testSlot struct {
	key      *hostkey.HostKey
	acc      *plan.AccountPlan
	upstream []limits.Counter
}

func accountPlan(id string) *plan.AccountPlan {
	return &plan.AccountPlan{Account: &account.Account{Meta: meta.Metadata{ID: id}}, Cooldown: keypool.NewCooldownCell()}
}

// buildHop is one pool at upstreamHost with sel over slots; every slot fits caller first.
func buildHop(sel keypool.KeySelection, caller []limits.Counter, slots ...testSlot) plan.RouteHop {
	pp := &plan.PoolPlan{Pool: &pool.Pool{Meta: meta.Metadata{ID: "pool-1"}}, Host: upstreamHost, Selection: sel}
	hop := plan.RouteHop{Pool: pp, Host: upstreamHost, CallerN: len(caller)}
	for _, s := range slots {
		ks := &plan.KeySlot{Credential: s.key, Host: upstreamHost, Account: s.acc}
		pp.Slots = append(pp.Slots, ks)
		hop.Slots = append(hop.Slots, ks)
		hop.Credentials = append(hop.Credentials, s.key)
		hop.Counters = append(hop.Counters, append(append([]limits.Counter(nil), caller...), s.upstream...))
	}
	return hop
}

func testKey(id string) *hostkey.HostKey {
	return &hostkey.HostKey{Meta: meta.Metadata{ID: id, Name: id}, Resolved: "sk-" + id, KeyHash: "hash-" + id}
}

func perMinute(id string, meter appratelimit.Meter, strategy appratelimit.Strategy, amount int64) *appratelimit.RateLimit {
	return &appratelimit.RateLimit{Meta: meta.Metadata{ID: id, Name: id}, Spec: appratelimit.Spec{Rules: []appratelimit.Rule{
		{Meter: meter, Amount: amount, Window: appratelimit.Window(time.Minute), Strategy: strategy},
	}}}
}
