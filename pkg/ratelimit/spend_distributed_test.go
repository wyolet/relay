//go:build integration

package ratelimit

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestSpend_RedisStore(t *testing.T) {
	runSpendSuite(t, "RedisStore", redisLimiterFactory(kvtest.Config(t)))
}
