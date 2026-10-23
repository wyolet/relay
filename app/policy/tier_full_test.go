package policy

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/meta"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// A key whose tier refused is skipped for that model until the refusal's
// retry-after, then picked again; another model may still use it meanwhile.
func TestAcquire_FullTierExpires(t *testing.T) {
	oneRequest := requestsRule()
	oneRequest.Amount = 1
	f := newAcquireFixture(t, oneRequest)
	now := time.Now()
	f.svc.clock = func() time.Time { return now }
	ctx := context.Background()

	first, err := f.svc.Acquire(ctx, f.in)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	_ = f.svc.Commit(ctx, first, pkgratelimit.Observations{})
	if _, err := f.svc.Acquire(ctx, f.in); !errors.Is(err, ErrSaturated) {
		t.Fatalf("second Acquire err = %v, want ErrSaturated", err)
	}

	reserves := len(f.store.Named("limit.reserve"))
	_, err = f.svc.Acquire(ctx, f.in)
	var exceeded *pkgratelimit.KeyQuotaExhausted
	if !errors.Is(err, ErrKeyTiersFull) || !errors.As(err, &exceeded) {
		t.Fatalf("Acquire on the full key = %v, want ErrKeyTiersFull", err)
	}
	if exceeded.RetryAfter <= 0 || exceeded.RetryAfter > time.Minute {
		t.Fatalf("retry after = %v, want within the window", exceeded.RetryAfter)
	}
	if got := len(f.store.Named("limit.reserve")); got != reserves {
		t.Fatalf("a remembered full key was reserved again (%d scripts, want %d)", got, reserves)
	}

	other := f.in
	other.Model = nil
	if _, err := f.svc.Acquire(ctx, other); errors.Is(err, ErrKeyTiersFull) {
		t.Fatal("a refusal for one model skipped the key for another")
	}

	now = now.Add(exceeded.RetryAfter)
	acq, err := f.svc.Acquire(ctx, f.in)
	if errors.Is(err, ErrKeyTiersFull) || acq == nil || acq.Key != f.key {
		t.Fatalf("after the retry-after: acq %+v err %v, want the key picked again", acq, err)
	}
}

func TestTierFull_Record(t *testing.T) {
	now := time.Now()
	keys := []*hostkey.HostKey{{Meta: meta.Metadata{ID: "a"}}, {Meta: meta.Metadata{ID: "b"}}}
	refusal := func(meter string, after time.Duration) *pkgratelimit.KeyQuotaExhausted {
		return &pkgratelimit.KeyQuotaExhausted{Rule: pkgratelimit.Rule{Meter: meter}, RetryAfter: after}
	}

	var full tierFull
	full.record("a", "m", refusal("requests", 0), now)
	if _, exceeded := full.exclude(keys, nil, "m", now); exceeded != nil {
		t.Fatal("a refusal with no retry-after was remembered")
	}

	full.record("a", "m", refusal("requests", 30*time.Second), now)
	full.record("b", "m", refusal("requests", 10*time.Second), now)
	excluded, exceeded := full.exclude(keys, nil, "m", now)
	if len(excluded) != 2 || exceeded == nil || exceeded.RetryAfter != 10*time.Second {
		t.Fatalf("exclude = %d keys, %+v; want both, retry after the earliest (10s)", len(excluded), exceeded)
	}

	var con tierFull
	con.record("a", "m", refusal("concurrency", time.Hour), now)
	if _, exceeded := con.exclude(keys, nil, "m", now); exceeded == nil || exceeded.RetryAfter != concurrencyFullFor {
		t.Fatalf("concurrency refusal remembered for %+v, want %v", exceeded, concurrencyFullFor)
	}
}

// The map stays bounded: expired entries make room, live ones are never evicted.
func TestTierFull_Bounded(t *testing.T) {
	now := time.Now()
	var full tierFull
	for i := range tierFullCap {
		full.record(strconv.Itoa(i), "m", &pkgratelimit.KeyQuotaExhausted{RetryAfter: time.Second}, now)
	}
	full.record("late", "m", &pkgratelimit.KeyQuotaExhausted{RetryAfter: time.Second}, now)
	if n := len(full.entries); n != tierFullCap {
		t.Fatalf("entries = %d, want the cap %d", n, tierFullCap)
	}
	later := now.Add(2 * time.Second)
	full.record("late", "m", &pkgratelimit.KeyQuotaExhausted{RetryAfter: time.Second}, later)
	if n := len(full.entries); n != 1 {
		t.Fatalf("entries = %d after the rest expired, want 1", n)
	}
}
