//go:build integration

package ratelimit

// scope_distributed_test.go runs the scope/revocation contract against a real
// Redis, so the Lua implementation and the mem emulator are held to the same
// behaviour. Run with: make test-integration.

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestContractScope_RedisStore(t *testing.T) {
	runScopeContractSuite(t, "RedisStore", redisLimiterFactory(kvtest.Config(t)))
}
