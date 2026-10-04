//go:build integration

package ratelimit

// scope_distributed_test.go runs the scope/revocation contract against a real
// Redis, so the Lua implementation and the mem emulator are held to the same
// behaviour. Needs Docker: go test -tags integration ./pkg/ratelimit/...

import "testing"

func TestContractScope_RedisStore(t *testing.T) {
	addr := startRedis(t)
	runScopeContractSuite(t, "RedisStore", redisLimiterFactory(addr))
}
