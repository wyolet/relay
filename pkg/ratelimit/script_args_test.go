package ratelimit

import (
	"strconv"
	"testing"
	"time"
)

// wireArgs renders script arguments as go-redis writes them: string and []byte both as their bytes, integers in decimal.
func wireArgs(t *testing.T, args []any) []string {
	t.Helper()
	out := make([]string, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case string:
			out[i] = v
		case []byte:
			out[i] = string(v)
		case int64:
			out[i] = strconv.FormatInt(v, 10)
		case int:
			out[i] = strconv.Itoa(v)
		default:
			t.Fatalf("arg %d: unexpected type %T", i, a)
		}
	}
	return out
}

func assertWire(t *testing.T, name string, gotKeys []string, gotArgs []any, wantKeys, wantArgs []string) {
	t.Helper()
	args := wireArgs(t, gotArgs)
	if len(gotKeys) != len(wantKeys) || len(args) != len(wantArgs) {
		t.Errorf("%s:\n got keys %#v\n got args %#v\nwant keys %#v\nwant args %#v", name, gotKeys, args, wantKeys, wantArgs)
		return
	}
	for i := range wantKeys {
		if gotKeys[i] != wantKeys[i] {
			t.Errorf("%s: key %d = %q, want %q", name, i, gotKeys[i], wantKeys[i])
		}
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("%s: arg %d = %q, want %q", name, i, args[i], wantArgs[i])
		}
	}
}

func scriptArgRules() []Rule {
	return []Rule{
		{Key: "P:rl:req", Name: "requests <on> rl & co", Meter: "requests", Amount: 100, Window: time.Minute},
		{Key: "P:rl:sw", Name: "sliding", Meter: "requests", Amount: 5, Window: time.Hour, Strategy: StrategySlidingWindow},
		{Key: "P:rl:fw", Name: "fixed", Meter: "requests", Amount: 7, Window: 10 * time.Second, Strategy: StrategyFixedWindow},
		{Key: "P:rl:lb", Name: "leaky", Meter: "requests", Amount: 3, Window: time.Second, Strategy: StrategyLeakyBucket},
		{Key: "P:rl:sess", Name: "session", Meter: "requests", Amount: 9, Window: 5 * time.Hour, Strategy: StrategySessionWindow},
		{Key: "P:rl:tok", Name: "tokens", Meter: "tokens", Amount: 1 << 40, Window: time.Hour, Strategy: StrategyTokenBucket},
		{Key: "P:rl:in", Name: "input", Meter: "tokens.input", Amount: 1000, Window: 24 * time.Hour},
		{Key: "P:rl:con", Name: "con", Meter: "concurrency", Amount: 4, Window: time.Minute},
		{Key: "T:jti:revoked", Name: "revoked", Meter: MeterRevoked},
	}
}

// TestScriptArgs_Golden pins the keys and arguments the reserve and commit scripts receive, as the store sees them.
func TestScriptArgs_Golden(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 30, 15, 123000000, time.UTC)
	l := New(newTestStore(t), discardLog(), func() time.Time { return now })
	rules := scriptArgRules()

	call, err := l.buildReserveCall("policy:p", rules, "tok-1")
	if err != nil {
		t.Fatal(err)
	}
	assertWire(t, "reserve", call.Keys, call.Args,
		[]string{"limit:{policy:p}:tb:P:rl:req:requests", "limit:{policy:p}:P:rl:sw:requests:2026-10-26T10:00:00Z", "limit:{policy:p}:P:rl:sw:requests:2026-10-26T09:00:00Z", "limit:{policy:p}:fw:P:rl:fw:requests:1793010610000", "limit:{policy:p}:lb:P:rl:lb:requests", "limit:{policy:p}:sw:P:rl:sess:requests", "limit:{policy:p}:P:rl:tok:tokens:2026-10-26T10:00:00Z", "limit:{policy:p}:P:rl:tok:tokens:2026-10-26T09:00:00Z", "limit:{policy:p}:P:rl:in:tokens.input:2026-10-26T00:00:00Z", "limit:{policy:p}:P:rl:in:tokens.input:2026-10-25T00:00:00Z", "limit:{policy:p}:lease:P:rl:con:concurrency", "limit:{policy:p}:T:jti:revoked", "limit:{policy:p}:committed:tok-1"},
		[]string{"1793010615123", "[{\"meter\":\"requests\",\"amount\":100,\"window_ms\":60000,\"strategy\":\"token-bucket\",\"rule_key\":\"P:rl:req\",\"rule_name\":\"requests \\u003con\\u003e rl \\u0026 co\",\"tb_key_idx\":1},{\"meter\":\"requests\",\"amount\":5,\"window_ms\":3600000,\"strategy\":\"sliding-window\",\"rule_key\":\"P:rl:sw\",\"rule_name\":\"sliding\",\"cur_key_idx\":2,\"prev_key_idx\":3},{\"meter\":\"requests\",\"amount\":7,\"window_ms\":10000,\"strategy\":\"fixed-window\",\"rule_key\":\"P:rl:fw\",\"rule_name\":\"fixed\",\"fw_key_idx\":4},{\"meter\":\"requests\",\"amount\":3,\"window_ms\":1000,\"strategy\":\"leaky-bucket\",\"rule_key\":\"P:rl:lb\",\"rule_name\":\"leaky\",\"lb_key_idx\":5},{\"meter\":\"requests\",\"amount\":9,\"window_ms\":18000000,\"strategy\":\"session-window\",\"rule_key\":\"P:rl:sess\",\"rule_name\":\"session\",\"sw_key_idx\":6},{\"meter\":\"tokens\",\"amount\":1099511627776,\"window_ms\":3600000,\"strategy\":\"sliding-window\",\"rule_key\":\"P:rl:tok\",\"rule_name\":\"tokens\",\"cur_key_idx\":7,\"prev_key_idx\":8},{\"meter\":\"tokens.input\",\"amount\":1000,\"window_ms\":86400000,\"strategy\":\"sliding-window\",\"rule_key\":\"P:rl:in\",\"rule_name\":\"input\",\"cur_key_idx\":9,\"prev_key_idx\":10},{\"meter\":\"concurrency\",\"amount\":4,\"window_ms\":60000,\"strategy\":\"token-bucket\",\"rule_key\":\"P:rl:con\",\"rule_name\":\"con\",\"con_key_idx\":11},{\"meter\":\"revoked\",\"amount\":0,\"window_ms\":0,\"strategy\":\"token-bucket\",\"rule_key\":\"T:jti:revoked\",\"rule_name\":\"revoked\",\"deny_key_idx\":12}]", "tok-1", "13"})

	res := &Reservation{ID: "tok-1", scope: "policy:p", rules: rules,
		conKeys:  []string{concurrencyKey("policy:p", rules[7])},
		tokRules: []Rule{rules[5], rules[6]},
		tbRules:  []Rule{rules[0], rules[5]},
		lbRules:  []Rule{rules[3], rules[2]},
		swRules:  []Rule{rules[4], rules[1]},
	}
	obs := Observations{Tokens: map[string]int64{"input": 120, "output": 40, "cache_read": 9}}
	call, err = l.buildCommitCall(res, obs, now)
	if err != nil {
		t.Fatal(err)
	}
	commitKeys := []string{"limit:{policy:p}:committed:tok-1", "limit:{policy:p}:lease:P:rl:con:concurrency", "limit:{policy:p}:P:rl:tok:tokens:2026-10-26T10:00:00Z", "limit:{policy:p}:P:rl:in:tokens.input:2026-10-26T00:00:00Z", "limit:{policy:p}:tb:P:rl:req:requests", "limit:{policy:p}:tb:P:rl:tok:tokens", "limit:{policy:p}:lb:P:rl:lb:requests", "limit:{policy:p}:lb:P:rl:fw:requests", "limit:{policy:p}:sw:P:rl:sess:requests", "limit:{policy:p}:sw:P:rl:sw:requests"}
	assertWire(t, "commit", call.Keys, call.Args, commitKeys,
		[]string{"tok-1", "60000", "1", "2", "[160,120]", "172800000", "0", "[[5,1000,100],[6,1000,1099511627776]]", "[[7,1000],[8,1000]]", "[[9,1],[10,1]]"})

	call, err = l.buildCommitCall(res, Observations{Cancelled: true, Tokens: obs.Tokens}, now)
	if err != nil {
		t.Fatal(err)
	}
	assertWire(t, "commit cancelled", call.Keys, call.Args, commitKeys,
		[]string{"tok-1", "60000", "1", "2", "[0,0]", "172800000", "1", "[[5,1000,100],[6,1000,1099511627776]]", "[[7,1000],[8,1000]]", "[[9,1],[10,1]]"})

	call, err = l.buildCommitCall(&Reservation{ID: "tok-2", scope: "s"}, Observations{}, now)
	if err != nil {
		t.Fatal(err)
	}
	assertWire(t, "commit empty", call.Keys, call.Args, []string{"limit:{s}:committed:tok-2"},
		[]string{"tok-2", "60000", "0", "0", "[]", "0", "0", "[]", "[]", "[]"})
}
