package ratelimit

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// hashTagRE matches a Redis key that contains exactly one valid hash tag:
// no curly braces outside the tag, one {...} segment with no nested braces.
var hashTagRE = regexp.MustCompile(`^[^{}]*\{[^{}]+\}[^{}]*$`)

func TestBucketKey_HashTag(t *testing.T) {
	r := Rule{
		Key:    "Policy:prod-policy:per-minute",
		Meter:  "requests",
		Amount: 100,
		Window: time.Minute,
	}
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	key := bucketKey("policy:prod-policy", r, ts)

	if !hashTagRE.MatchString(key) {
		t.Errorf("bucketKey does not match hash-tag pattern: %q", key)
	}
}

func TestConcurrencyKey_HashTag(t *testing.T) {
	r := Rule{
		Key:    "Secret:sk-abc123:max-concurrent",
		Meter:  "concurrency",
		Amount: 10,
		Window: time.Minute,
	}
	key := concurrencyKey("policy:prod-policy", r)

	if !hashTagRE.MatchString(key) {
		t.Errorf("concurrencyKey does not match hash-tag pattern: %q", key)
	}
}

// TestKeyBuilders_Golden pins every key byte for byte: a changed key orphans live counters in the store.
func TestKeyBuilders_Golden(t *testing.T) {
	plus5 := time.FixedZone("plus5", 5*60*60)
	minus3 := time.FixedZone("minus3", -3*60*60-30*60)
	reqRule := Rule{Key: "Policy:prod-policy:per-minute", Meter: "requests"}
	tokRule := Rule{Key: "Policy:0192f3a1-7c2e-7b1d-9a4e-5f6a7b8c9d0e:rl-1:tokens.input", Meter: "tokens.input"}
	oddRule := Rule{Key: "Héllo:ключ:%s:%d", Meter: "tokens"}
	emptyRule := Rule{}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{"bucket utc", bucketKey("policy:prod-policy", reqRule, time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)),
			"limit:{policy:prod-policy}:Policy:prod-policy:per-minute:requests:2024-01-15T10:00:00Z"},
		{"bucket nanos dropped", bucketKey("policy:prod-policy", reqRule, time.Date(2026, 10, 26, 23, 59, 59, 999999999, time.UTC)),
			"limit:{policy:prod-policy}:Policy:prod-policy:per-minute:requests:2026-10-26T23:59:59Z"},
		{"bucket east zone to utc", bucketKey("hostkey:0192f3a1", tokRule, time.Date(2026, 1, 1, 3, 4, 5, 0, plus5)),
			"limit:{hostkey:0192f3a1}:Policy:0192f3a1-7c2e-7b1d-9a4e-5f6a7b8c9d0e:rl-1:tokens.input:tokens.input:2025-12-31T22:04:05Z"},
		{"bucket west zone to utc", bucketKey("s", oddRule, time.Date(2026, 2, 28, 22, 0, 0, 0, minus3)),
			"limit:{s}:Héllo:ключ:%s:%d:tokens:2026-03-01T01:30:00Z"},
		{"bucket small year", bucketKey("s", reqRule, time.Date(99, 3, 4, 5, 6, 7, 0, time.UTC)),
			"limit:{s}:Policy:prod-policy:per-minute:requests:0099-03-04T05:06:07Z"},
		{"bucket zero time", bucketKey("", emptyRule, time.Time{}),
			"limit:{}:::0001-01-01T00:00:00Z"},
		{"bucket long scope", bucketKey(strings.Repeat("x", 200), reqRule, time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)),
			"limit:{" + strings.Repeat("x", 200) + "}:Policy:prod-policy:per-minute:requests:2024-01-15T10:00:00Z"},
		{"concurrency", concurrencyKey("policy:prod-policy", Rule{Key: "Secret:sk-abc123:max-concurrent", Meter: "concurrency"}),
			"limit:{policy:prod-policy}:lease:Secret:sk-abc123:max-concurrent:concurrency"},
		{"fixed window", fixedWindowKey("policy:prod-policy", reqRule, 1761474600000),
			"limit:{policy:prod-policy}:fw:Policy:prod-policy:per-minute:requests:1761474600000"},
		{"fixed window zero", fixedWindowKey("s", emptyRule, 0), "limit:{s}:fw:::0"},
		{"fixed window negative", fixedWindowKey("s", reqRule, -60000),
			"limit:{s}:fw:Policy:prod-policy:per-minute:requests:-60000"},
		{"token bucket", tbStateKey("policy:prod-policy", reqRule),
			"limit:{policy:prod-policy}:tb:Policy:prod-policy:per-minute:requests"},
		{"leaky bucket", lbStateKey("policy:prod-policy", tokRule),
			"limit:{policy:prod-policy}:lb:Policy:0192f3a1-7c2e-7b1d-9a4e-5f6a7b8c9d0e:rl-1:tokens.input:tokens.input"},
		{"session window", swStateKey("s", oddRule), "limit:{s}:sw:Héllo:ключ:%s:%d:tokens"},
		{"deny", denyKey("token:jti-1", Rule{Key: "Token:jti-1:revoked", Meter: MeterRevoked}),
			"limit:{token:jti-1}:Token:jti-1:revoked"},
		{"commit guard", commitGuardKey("policy:prod-policy", "res-uuid-1234"),
			"limit:{policy:prod-policy}:committed:res-uuid-1234"},
		{"commit guard empty", commitGuardKey("", ""), "limit:{}:committed:"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestCommitGuardKey_HashTag(t *testing.T) {
	key := commitGuardKey("policy:prod-policy", "res-uuid-1234")

	if !hashTagRE.MatchString(key) {
		t.Errorf("commitGuardKey does not match hash-tag pattern: %q", key)
	}
}

// The longest key a policy rule produces in practice (UUID scope, policy, rate limit and model ids) still builds in one allocation.
func TestWindowKeys_LongRuleKeyAllocatesOnce(t *testing.T) {
	const id = "0192f3a1-7c2e-7b1d-9a4e-5f6a7b8c9d0e"
	r := Rule{
		Key:   "policy:" + id + ":rl:" + id + ":m:" + id + ":session-window:2592000:tokens.server_tool_use_output",
		Meter: "tokens.server_tool_use_output",
	}
	scope := "policy:" + id
	ts := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	if n := testing.AllocsPerRun(100, func() { _ = bucketKey(scope, r, ts) }); n != 1 {
		t.Errorf("bucketKey allocs = %v, want 1", n)
	}
	if n := testing.AllocsPerRun(100, func() { _ = fixedWindowKey(scope, r, ts.UnixMilli()) }); n != 1 {
		t.Errorf("fixedWindowKey allocs = %v, want 1", n)
	}
}
