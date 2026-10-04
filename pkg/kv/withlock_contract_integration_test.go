//go:build integration

package kv_test

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// Redis leg of the WithLock blocking contract (see
// withlock_contract_test.go).
//
// Audit 2026-07-04 (P1 tracker #9): Redis.WithLock was non-blocking
// (ErrLockBusy under contention without running fn); it now polls
// SET NX PX with jittered backoff until acquired or ctx is done.
func TestWithLockBlockingContract_Redis(t *testing.T) {
	withLockBlockingContract(t, newRedisStore(t, kvtest.Config(t)))
}
