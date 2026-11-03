package lua

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/wyolet/relay/app/budget"
	"github.com/wyolet/relay/app/limits"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// group is one atomicity group's slice of a request's counters, reserved in one script.
type group struct {
	name     string
	upstream bool
	rules    []pkgratelimit.Rule
}

// maxGroups covers a request's usual groups (caller, account, host) without a heap allocation for the order.
const maxGroups = 4

// groupCounters splits cs by Group: caller groups first, then upstream groups, each in the order the list first names it. A counter whose ID the group already holds is dropped, so equal IDs count once.
func groupCounters(cs []limits.Counter) ([]group, error) {
	var buf [maxGroups]string
	names := buf[:0]
	upstreamAt := 0
	for pass := range 2 {
		for i := range cs {
			if cs[i].Rung.Upstream() != (pass == 1) || containsName(names, cs[i].Group) {
				continue
			}
			names = append(names, cs[i].Group)
		}
		if pass == 0 {
			upstreamAt = len(names)
		}
	}

	all := make([]pkgratelimit.Rule, 0, len(cs))
	groups := make([]group, len(names))
	for gi, name := range names {
		start := len(all)
		for i := range cs {
			c := &cs[i]
			if c.Group != name || holdsKey(all[start:], ruleKey(c)) {
				continue
			}
			r, err := toRule(c)
			if err != nil {
				return nil, err
			}
			all = append(all, r)
		}
		groups[gi] = group{name: name, upstream: gi >= upstreamAt, rules: all[start:len(all):len(all)]}
	}
	return groups, nil
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

func holdsKey(rules []pkgratelimit.Rule, key string) bool {
	for i := range rules {
		if rules[i].Key == key {
			return true
		}
	}
	return false
}

// toRule renders c as the limiter rule that counts it. The name is the ID: it only labels refusals and logs.
func toRule(c *limits.Counter) (pkgratelimit.Rule, error) {
	switch c.Kind {
	case limits.KindRule:
		return pkgratelimit.Rule{
			Key: ruleKey(c), Name: c.ID, Meter: string(c.Rule.Meter), Strategy: pkgratelimit.Strategy(c.Rule.Strategy),
			Amount: c.Rule.Amount, Window: c.Rule.Window.Duration(),
		}, nil
	case limits.KindBudget:
		if c.Budget == nil {
			return pkgratelimit.Rule{}, fmt.Errorf("limits: budget counter %s has no budget", c.ID)
		}
		capNanos, err := usdNanos(c.Budget.Spec.AmountUSD)
		if err != nil {
			return pkgratelimit.Rule{}, fmt.Errorf("limits: budget counter %s: %w", c.ID, err)
		}
		period := c.Budget.Spec.Period
		if period == "" {
			period = budget.DefaultPeriod
		}
		return pkgratelimit.Rule{
			Key: ruleKey(c), Name: c.ID, Meter: pkgratelimit.MeterSpend, Amount: capNanos,
			Period: pkgratelimit.Period(period), Alert: c.Budget.Spec.Mode == budget.ModeAlert,
		}, nil
	}
	return pkgratelimit.Rule{}, fmt.Errorf("limits: counter %s has unknown kind %d", c.ID, c.Kind)
}

const nanosPerUSD = 1_000_000_000

var errAmount = errors.New("amount must be a non-negative decimal of USD with at most 9 fractional digits")

// usdNanos parses a decimal USD amount into nanos without a float, so a cap is exact.
func usdNanos(s string) (int64, error) {
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 9 || whole == "" || whole[0] == '-' || whole[0] == '+' {
		return 0, errAmount
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w > math.MaxInt64/nanosPerUSD-1 {
		return 0, errAmount
	}
	var f int64
	if frac != "" {
		if frac[0] == '-' || frac[0] == '+' {
			return 0, errAmount
		}
		if f, err = strconv.ParseInt(frac, 10, 64); err != nil {
			return 0, errAmount
		}
		for range 9 - len(frac) {
			f *= 10
		}
	}
	return w*nanosPerUSD + f, nil
}
