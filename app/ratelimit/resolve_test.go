package ratelimit

import (
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
)

func rule(m Meter, s Strategy, w time.Duration, amount int64) Rule {
	return Rule{Meter: m, Strategy: s, Window: Window(w), Amount: amount}
}

// keysByContent resolves rules and maps each rule's identity (meter, strategy, window) to the counter key it got.
func keysByContent(t *testing.T, rules ...Rule) map[string]string {
	t.Helper()
	rl := &RateLimit{Spec: Spec{Rules: rules}}
	out := map[string]string{}
	for i, r := range ResolveWithScope("policy", "pol-1:rl:rl-1", rl) {
		src := rules[i]
		out[string(src.Meter)+"|"+string(src.Strategy)+"|"+src.Window.Duration().String()] = r.Key
	}
	return out
}

func TestResolveWithScope_KeyFormat(t *testing.T) {
	rl := &RateLimit{Spec: Spec{Rules: []Rule{rule(MeterTokensInput, StrategySlidingWindow, time.Hour, 5)}}}
	got := ResolveWithScope("proxy", "abc", rl)
	if len(got) != 1 || got[0].Key != "proxy:abc:sliding-window:3600:tokens.input" {
		t.Fatalf("keys = %+v, want proxy:abc:sliding-window:3600:tokens.input", got)
	}
}

// A rule keeps its counter when other rules move around it, are added or are removed: the key comes from its content, not its position.
func TestResolveWithScope_KeyIgnoresPosition(t *testing.T) {
	rpm := rule(MeterRequests, StrategySlidingWindow, time.Minute, 60)
	tpm := rule(MeterTokens, StrategySlidingWindow, time.Minute, 1000)
	rpd := rule(MeterRequests, StrategyFixedWindow, 24*time.Hour, 5000)

	base := keysByContent(t, rpm, tpm, rpd)
	for name, rules := range map[string][]Rule{
		"reordered": {rpd, rpm, tpm},
		"inserted":  {rule(MeterConcurrency, StrategySlidingWindow, time.Minute, 4), rpm, tpm, rpd},
		"deleted":   {tpm, rpd},
	} {
		for id, key := range keysByContent(t, rules...) {
			if want, ok := base[id]; ok && key != want {
				t.Errorf("%s: rule %s moved from %q to %q", name, id, want, key)
			}
		}
	}
}

func TestResolveWithScope_KeyFollowsIdentity(t *testing.T) {
	key := func(r Rule) string {
		return ResolveWithScope("policy", "pol-1:rl:rl-1", &RateLimit{Spec: Spec{Rules: []Rule{r}}})[0].Key
	}
	orig := rule(MeterRequests, StrategySlidingWindow, time.Minute, 60)
	if amount := rule(MeterRequests, StrategySlidingWindow, time.Minute, 600); key(amount) != key(orig) {
		t.Errorf("amount edit moved the counter: %q -> %q", key(orig), key(amount))
	}
	if window := rule(MeterRequests, StrategySlidingWindow, time.Hour, 60); key(window) == key(orig) {
		t.Errorf("window edit kept the counter %q", key(orig))
	}
	if strategy := rule(MeterRequests, StrategyFixedWindow, time.Minute, 60); key(strategy) == key(orig) {
		t.Errorf("strategy edit kept the counter %q", key(orig))
	}
}

func TestValidate_DuplicateRules(t *testing.T) {
	rl := func(rules ...Rule) *RateLimit {
		return &RateLimit{
			Meta: meta.Metadata{Name: "rl", Owner: meta.Owner{Kind: meta.OwnerUser}},
			Spec: Spec{Rules: rules},
		}
	}
	dup := rl(
		rule(MeterRequests, StrategySlidingWindow, time.Minute, 60),
		rule(MeterTokens, StrategySlidingWindow, time.Minute, 1000),
		rule(MeterRequests, StrategySlidingWindow, 60*time.Second, 100),
	)
	err := dup.Validate()
	if err == nil || !strings.Contains(err.Error(), "rules[0] and rules[2]") {
		t.Fatalf("err = %v, want rules[0] and rules[2] rejected as one counter", err)
	}

	distinct := rl(
		rule(MeterRequests, StrategySlidingWindow, time.Minute, 60),
		rule(MeterRequests, StrategySlidingWindow, time.Hour, 600),
		rule(MeterRequests, StrategyFixedWindow, time.Minute, 60),
		rule(MeterTokens, StrategySlidingWindow, time.Minute, 60),
	)
	if err := distinct.Validate(); err != nil {
		t.Fatalf("rules differing in window, strategy or meter: %v", err)
	}
}

// BenchmarkResolveWithScope pins the per-rule cost: the key and the name.
func BenchmarkResolveWithScope(b *testing.B) {
	rl := &RateLimit{Spec: Spec{Rules: []Rule{rule(MeterRequests, StrategySlidingWindow, time.Hour, 60)}}}
	b.ReportAllocs()
	for b.Loop() {
		_ = ResolveWithScope("proxy", "0123456789abcdef0123456789abcdef", rl)
	}
}
