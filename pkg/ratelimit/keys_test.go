package ratelimit

import (
	"regexp"
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

func TestCommitGuardKey_HashTag(t *testing.T) {
	key := commitGuardKey("policy:prod-policy", "res-uuid-1234")

	if !hashTagRE.MatchString(key) {
		t.Errorf("commitGuardKey does not match hash-tag pattern: %q", key)
	}
}
