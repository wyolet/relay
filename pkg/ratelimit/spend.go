package ratelimit

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// spendKeyGrace keeps a closed period's count past its end, so a pod whose clock lags still reads it.
const spendKeyGrace = time.Hour

// periodBounds returns the UTC calendar period p containing t.
func periodBounds(p Period, t time.Time) (start, end time.Time, err error) {
	t = t.UTC()
	y, m, d := t.Date()
	switch p {
	case PeriodDay:
		start = time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 0, 1), nil
	case PeriodWeek:
		start = time.Date(y, m, d-(int(t.Weekday())+6)%7, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 0, 7), nil
	case PeriodMonth:
		start = time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		return start, start.AddDate(0, 1, 0), nil
	}
	return time.Time{}, time.Time{}, fmt.Errorf("limit: unknown spend period %q", p)
}

// onlyAlerts reports whether rules is non-empty and holds nothing but Alert spend rules, which Reserve does not check.
func onlyAlerts(rules []Rule) bool {
	for _, r := range rules {
		if r.Meter != MeterSpend || !r.Alert {
			return false
		}
	}
	return len(rules) > 0
}

// appendSpends appends the commit script's spend descriptors, [[key_idx, ttl_ms, cap], ...], adding each rule's key for the period containing now to keys.
func appendSpends(buf []byte, keys []string, res *Reservation, now time.Time) ([]byte, []string, error) {
	buf = append(buf, '[')
	for i, rule := range res.spendRules {
		start, end, err := periodBounds(rule.Period, now)
		if err != nil {
			return nil, nil, err
		}
		keys = append(keys, spendKey(res.scope, rule, start))
		buf = appendRefund(buf, i, int64(len(keys)), end.Add(spendKeyGrace).Sub(now).Milliseconds())
		buf = append(buf, ',')
		buf = strconv.AppendInt(buf, rule.Amount, 10)
		buf = append(buf, ']')
	}
	return append(buf, ']'), keys, nil
}

// crossedSpends returns the spend rules a commit reply names as reaching their cap with this charge. The reply is "ok" or "ok:<i>,<j>" with 1-based indices into res.spendRules.
func crossedSpends(raw []byte, res *Reservation) []Rule {
	list, ok := strings.CutPrefix(string(raw), "ok:")
	if !ok {
		return nil
	}
	var out []Rule
	for s := range strings.SplitSeq(list, ",") {
		i, err := strconv.Atoi(s)
		if err == nil && i >= 1 && i <= len(res.spendRules) {
			out = append(out, res.spendRules[i-1])
		}
	}
	return out
}
