//go:build integration

package kv_test

import "testing"

// Redis leg of the WithLock blocking contract (see
// withlock_contract_test.go). Uses the same testcontainers harness as
// the rest of the Redis contract suite in redis_test.go.
//
// Audit 2026-07-04 (P1 tracker #9): Redis.WithLock was non-blocking
// (ErrLockBusy under contention without running fn); it now polls
// SET NX PX with jittered backoff until acquired or ctx is done.
func TestWithLockBlockingContract_Redis(t *testing.T) {
	addr := startRedis(t)
	withLockBlockingContract(t, newRedisStore(t, addr))
}
