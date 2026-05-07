package ratelimit

import (
	"regexp"
	"testing"
	"time"

	"github.com/wyolet/relay/internal/catalog"
)

// hashTagRE matches a Redis key that contains exactly one valid hash tag:
// no curly braces outside the tag, one {...} segment with no nested braces.
var hashTagRE = regexp.MustCompile(`^[^{}]*\{[^{}]+\}[^{}]*$`)

func TestBucketKey_HashTag(t *testing.T) {
	r := catalog.ResolvedRule{
		ParentKind: catalog.KindPool,
		ParentName: "prod-pool",
		Meter:      catalog.MeterRequests,
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: "per-minute"},
			Spec: catalog.RateLimitSpec{
				Strategy: catalog.StrategySlidingWindow,
				Window:   60_000_000_000,
				Amount:   100,
			},
		},
	}
	ts := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	key := bucketKey("prod-pool", r, ts)

	// Sample: limit:{pool:prod-pool}:Pool:prod-pool:per-minute:requests:2024-01-15T10:00:00Z
	if !hashTagRE.MatchString(key) {
		t.Errorf("bucketKey does not match hash-tag pattern: %q", key)
	}
}

func TestConcurrencyKey_HashTag(t *testing.T) {
	r := catalog.ResolvedRule{
		ParentKind: catalog.KindSecret,
		ParentName: "sk-abc123",
		Meter:      catalog.MeterConcurrency,
		RateLimit: &catalog.RateLimit{
			Metadata: catalog.Metadata{Name: "max-concurrent"},
			Spec: catalog.RateLimitSpec{
				Strategy: catalog.StrategySlidingWindow,
				Window:   60_000_000_000,
				Amount:   10,
			},
		},
	}
	key := concurrencyKey("prod-pool", r)

	// Sample: limit:{pool:prod-pool}:Secret:sk-abc123:max-concurrent:concurrency
	if !hashTagRE.MatchString(key) {
		t.Errorf("concurrencyKey does not match hash-tag pattern: %q", key)
	}
}

func TestCommitGuardKey_HashTag(t *testing.T) {
	key := commitGuardKey("prod-pool", "res-uuid-1234")

	// Sample: limit:{pool:prod-pool}:committed:res-uuid-1234
	if !hashTagRE.MatchString(key) {
		t.Errorf("commitGuardKey does not match hash-tag pattern: %q", key)
	}
}
