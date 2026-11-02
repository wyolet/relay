package control

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/engine"
	"github.com/wyolet/relay/pkg/ids"
)

// AccountLimits is the caller half of the ladder a lane climbs, with current usage against every limit.
type AccountLimits struct {
	Lane     LimitsAccount   `json:"lane"     doc:"The account requests run in."`
	Rungs    []LimitsRung    `json:"rungs"    doc:"The accounts a request in the lane climbs, outermost first: the team's account, the project's account, then the lane. Every rung only narrows. Empty for a provider account, whose own policy caps its keys upstream and is not reported here."`
	Tightest *LimitsTightest `json:"tightest,omitempty" doc:"The hard limit with the least left as a share of its cap; omitted when no rung caps anything."`
}

// LimitsAccount names an account on the ladder.
type LimitsAccount struct {
	ID   string       `json:"id"   doc:"Account id."`
	Name string       `json:"name" doc:"Account slug."`
	Kind account.Kind `json:"kind" enum:"team,project,personal,member,service,provider" doc:"What the account is."`
}

// LimitsRung is one account on the ladder and what its policy links.
type LimitsRung struct {
	Account    LimitsAccount     `json:"account"    doc:"The account at this rung."`
	Policy     *BoundPolicy      `json:"policy,omitempty" doc:"The policy bound to the account; omitted when none is."`
	RateLimits []LimitsRateLimit `json:"rateLimits" doc:"The rate limits the policy links, disabled ones left out."`
	Budgets    []LimitsBudget    `json:"budgets"    doc:"The budgets the policy links."`
}

// LimitsRateLimit is one linked rate limit with each rule's usage.
type LimitsRateLimit struct {
	RateLimitID string       `json:"rateLimitId" doc:"Rate limit id."`
	Name        string       `json:"name"        doc:"Rate limit slug."`
	Models      []string     `json:"models"      doc:"The models the link applies to, in the policy's model grammar; empty applies it to every request."`
	Shared      bool         `json:"shared"      doc:"True counts once for the rung's account, so used is what every lane under it spent together; false counts once per lane, so used is this lane's own."`
	Rules       []LimitsRule `json:"rules"       doc:"The rate limit's rules, in order."`
}

// LimitsRule is one rule with its current count.
type LimitsRule struct {
	Meter     ratelimit.Meter    `json:"meter"     doc:"What the rule counts."`
	Strategy  ratelimit.Strategy `json:"strategy"  enum:"token-bucket,sliding-window,fixed-window,leaky-bucket,session-window" doc:"Counting algorithm. For a token bucket used is the tokens drawn and not yet refilled; for a leaky bucket its level."`
	Amount    int64              `json:"amount"    doc:"How much of the meter the window allows."`
	Window    int64              `json:"window"    doc:"Measurement period, in whole seconds."`
	Used      int64              `json:"used"      doc:"The current count as admission compares it, rounded up. Under the hybrid limiter it is this relay instance's view, which may lag other instances by up to one sync interval."`
	Remaining int64              `json:"remaining" doc:"amount minus used, never below zero."`
}

// LimitsBudget is one linked budget with this period's spend.
type LimitsBudget struct {
	BudgetID     string        `json:"budgetId"     doc:"Budget id."`
	Name         string        `json:"name"         doc:"Budget slug."`
	Shared       bool          `json:"shared"       doc:"True spends one budget for the rung's account, so spentUsd is every lane under it together; false gives each lane its own, so spentUsd is this lane's."`
	AmountUSD    string        `json:"amountUsd"    doc:"Spend cap in USD per period, as a decimal string."`
	Period       budget.Period `json:"period"       enum:"day,week,month" doc:"Calendar period the cap covers, in UTC."`
	Mode         budget.Mode   `json:"mode"         enum:"hard,alert" doc:"hard refuses requests once the cap is spent; alert only reports the overrun."`
	SpentUSD     string        `json:"spentUsd"     doc:"USD spent in the current period, as a decimal string."`
	RemainingUSD string        `json:"remainingUsd" doc:"amountUsd minus spentUsd, never below zero, as a decimal string."`
}

// LimitsTightest points at the limit closest to refusing.
type LimitsTightest struct {
	AccountID string  `json:"accountId" doc:"The account whose policy links the limit."`
	Kind      string  `json:"kind"      enum:"rateLimit,budget" doc:"Whether the limit is a rate-limit rule or a budget."`
	ID        string  `json:"id"        doc:"The rate limit's or the budget's id."`
	Remaining float64 `json:"remaining" doc:"What is left: in the rule's meter for a rate limit, in USD for a budget."`
	Label     string  `json:"label"     doc:"A short sentence for display, such as \"$50 left: team cap\"."`
}

type limitsOut struct {
	Body AccountLimits
}

func registerLimits(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "account_limits",
		Method:      http.MethodGet,
		Path:        "/accounts/{ref}/limits",
		Summary:     "Read the limits a request in this account climbs",
		Description: "The caller rungs a request in the account climbs (team account, project account, the account itself), each with its bound policy, the rate limits and budgets it links and current usage against them, and the tightest of them. Readable by whoever may read the account and by the user who acts in it. A team's or project's own account is a rung, not a lane: it answers 400. {ref} is a slug or id.",
		Tags:        []string{"accounts"},
		Middlewares: protect,
		Errors:      []int{400, 401, 404, 503},
	}, func(ctx context.Context, in *refInput) (*limitsOut, error) {
		v, err := planView(d)
		if err != nil {
			return nil, err
		}
		acc := byRef[account.Account](v, in.Ref)
		if acc == nil || !mayReadAccount(ctx, d.Authz, acc, in.Ref) {
			return nil, huma.Error404NotFound("account " + strconv.Quote(in.Ref) + " not found")
		}
		return laneLimits(ctx, d, v, acc)
	})

	huma.Register(api, huma.Operation{
		OperationID: "key_limits",
		Method:      http.MethodGet,
		Path:        "/keys/{ref}/limits",
		Summary:     "Read the limits of a key's default lane",
		Description: "The limits of the account the key's requests run in by default, as GET /accounts/{ref}/limits answers them. A request that switches lane climbs that account's limits instead. Readable by whoever may read the key and by the user it authenticates as. {ref} is a slug or id.",
		Tags:        []string{"keys"},
		Middlewares: protect,
		Errors:      []int{401, 404, 503},
	}, func(ctx context.Context, in *refInput) (*limitsOut, error) {
		v, err := planView(d)
		if err != nil {
			return nil, err
		}
		k := byRef[key.Key](v, in.Ref)
		if k == nil || !mayReadKey(ctx, d.Authz, k, in.Ref) {
			return nil, huma.Error404NotFound("key " + strconv.Quote(in.Ref) + " not found")
		}
		acc := engine.Get[account.Account](v, plan.KeyLane(v, k))
		if acc == nil {
			return nil, huma.Error404NotFound("the account key " + strconv.Quote(in.Ref) + " runs in is not made yet")
		}
		return laneLimits(ctx, d, v, acc)
	})
}

// byRef finds a row of kind T in v by id or, for a non-UUID ref, by slug.
func byRef[T any](v *engine.View, ref string) *T {
	if ids.Valid(ref) {
		if row := engine.Get[T](v, ref); row != nil {
			return row
		}
	}
	return engine.ByNameOf[T](v, ref)
}

// mayReadAccount allows the user acting in the account, then whoever the account read allows.
func mayReadAccount(ctx context.Context, az authz.Authorizer, a *account.Account, ref string) bool {
	if h := a.Spec.Holder; h != nil && h.Kind == account.HolderUser && isCaller(ctx, h.ID) {
		return true
	}
	return az.Authorize(ctx, "accounts.get", authz.Resource{Kind: "account", ID: a.Meta.ID, Name: ref, Owner: &a.Meta.Owner}) == nil
}

// mayReadKey allows the user the key authenticates as, then whoever the key read allows.
func mayReadKey(ctx context.Context, az authz.Authorizer, k *key.Key, ref string) bool {
	if k.Spec.Principal.Kind == key.PrincipalUser && isCaller(ctx, k.Spec.Principal.ID) {
		return true
	}
	return az.Authorize(ctx, "keys.get", authz.Resource{Kind: "key", ID: k.Meta.ID, Name: ref, Owner: &k.Meta.Owner}) == nil
}

func isCaller(ctx context.Context, userID string) bool {
	a := actor.From(ctx)
	return a != nil && a.UserID != "" && a.UserID == userID
}

func limitsAccount(a *account.Account) LimitsAccount {
	return LimitsAccount{ID: a.Meta.ID, Name: a.Meta.Name, Kind: a.DerivedKind()}
}

// laneLimits reads acc's caller rungs from its lane plan and their counts from the limiter.
func laneLimits(ctx context.Context, d Deps, v *engine.View, acc *account.Account) (*limitsOut, error) {
	switch acc.DerivedKind() {
	case account.KindTeam, account.KindProject:
		return nil, huma.Error400BadRequest("account " + acc.Meta.Name + " is a " + string(acc.DerivedKind()) + "'s own account: a rung of the lanes under it, not a lane; read the limits of an account or key in it")
	}
	out := &limitsOut{Body: AccountLimits{Lane: limitsAccount(acc), Rungs: []LimitsRung{}}}
	cp := plan.LanePlan(v, acc.Meta.ID)
	if cp == nil {
		return out, nil
	}
	rungs := cp.Limits()
	var cs []limits.Counter
	for _, r := range rungs {
		for _, rl := range r.RateLimits {
			cs = append(cs, rl.Counters...)
		}
		for _, b := range r.Budgets {
			cs = append(cs, b.Counter)
		}
	}
	readings := []limits.Reading{}
	if len(cs) > 0 {
		if d.Counters == nil {
			return nil, huma.Error503ServiceUnavailable("no limiter to read counts from")
		}
		var err error
		if readings, err = d.Counters.Peek(ctx, cs); err != nil {
			return nil, huma.Error503ServiceUnavailable("reading counts: " + err.Error())
		}
	}
	var pick tightest
	at := 0
	for _, r := range rungs {
		rung := LimitsRung{Account: limitsAccount(r.Rung.Account), RateLimits: []LimitsRateLimit{}, Budgets: []LimitsBudget{}}
		if ep := r.Rung.Policy; ep != nil {
			rung.Policy = &BoundPolicy{ID: ep.Policy.Meta.ID, Name: ep.Policy.Meta.Name}
		}
		kind := string(r.Rung.Account.DerivedKind())
		for _, rl := range r.RateLimits {
			l := rl.Link
			row := LimitsRateLimit{RateLimitID: l.RateLimit.Meta.ID, Name: l.RateLimit.Meta.Name, Models: l.Models, Shared: l.Shared, Rules: []LimitsRule{}}
			if row.Models == nil {
				row.Models = []string{}
			}
			for j, rule := range l.RateLimit.Spec.Rules {
				rd := readings[at+j]
				row.Rules = append(row.Rules, LimitsRule{
					Meter: rule.Meter, Strategy: rule.Strategy, Amount: rule.Amount, Window: int64(rule.Window.Duration() / time.Second),
					Used: rd.Used, Remaining: rd.Remaining(),
				})
				pick.offer(rd, func() LimitsTightest {
					return LimitsTightest{AccountID: r.Rung.Account.Meta.ID, Kind: "rateLimit", ID: row.RateLimitID, Remaining: float64(rd.Remaining()),
						Label: strconv.FormatInt(rd.Remaining(), 10) + " " + meterNoun(rule.Meter) + " left: " + kind + " " + row.Name}
				})
			}
			at += len(rl.Counters)
			rung.RateLimits = append(rung.RateLimits, row)
		}
		for _, b := range r.Budgets {
			rd, spec := readings[at], b.Link.Budget.Spec
			at++
			period, mode := spec.Period, spec.Mode
			if period == "" {
				period = budget.DefaultPeriod
			}
			if mode == "" {
				mode = budget.DefaultMode
			}
			row := LimitsBudget{
				BudgetID: b.Link.Budget.Meta.ID, Name: b.Link.Budget.Meta.Name, Shared: b.Link.Shared,
				AmountUSD: spec.AmountUSD, Period: period, Mode: mode, SpentUSD: usdString(rd.Used), RemainingUSD: usdString(rd.Remaining()),
			}
			rung.Budgets = append(rung.Budgets, row)
			if mode == budget.ModeAlert {
				continue
			}
			pick.offer(rd, func() LimitsTightest {
				return LimitsTightest{AccountID: r.Rung.Account.Meta.ID, Kind: "budget", ID: row.BudgetID, Remaining: float64(rd.Remaining()) / nanosPerUSD,
					Label: "$" + usdLabel(rd.Remaining()) + " left: " + kind + " " + row.Name}
			})
		}
		out.Body.Rungs = append(out.Body.Rungs, rung)
	}
	out.Body.Tightest = pick.best
	return out, nil
}

// tightest keeps the reading with the least left as a share of its cap; on a tie the earlier, outer one.
type tightest struct {
	share float64
	best  *LimitsTightest
}

func (t *tightest) offer(r limits.Reading, describe func() LimitsTightest) {
	if r.Amount <= 0 {
		return
	}
	share := float64(r.Remaining()) / float64(r.Amount)
	if t.best != nil && share >= t.share {
		return
	}
	d := describe()
	t.share, t.best = share, &d
}

// meterNoun is how a label counts a meter: "requests", "concurrent requests", "tokens", "input tokens".
func meterNoun(m ratelimit.Meter) string {
	switch m {
	case ratelimit.MeterRequests:
		return "requests"
	case ratelimit.MeterConcurrency:
		return "concurrent requests"
	case ratelimit.MeterTokens:
		return "tokens"
	}
	return strings.ReplaceAll(strings.TrimPrefix(string(m), "tokens."), "_", " ") + " tokens"
}

const nanosPerUSD = 1_000_000_000

// usdString renders USD nanos as an exact decimal, trailing zeros dropped: "12.5", "0".
func usdString(nanos int64) string {
	s := strconv.FormatInt(nanos/nanosPerUSD, 10)
	if frac := nanos % nanosPerUSD; frac != 0 {
		s += "." + strings.TrimRight(strconv.FormatInt(nanosPerUSD+frac, 10)[1:], "0")
	}
	return s
}

// usdLabel renders USD nanos rounded down to cents for display, cents dropped when whole: "50", "49.50".
func usdLabel(nanos int64) string {
	cents := nanos / (nanosPerUSD / 100)
	s := strconv.FormatInt(cents/100, 10)
	if c := cents % 100; c != 0 {
		s += "." + strconv.FormatInt(100+c, 10)[1:]
	}
	return s
}
