//go:build integration

package keypool

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func BenchmarkPickRoundRobinRedis(b *testing.B) {
	benchPickRoundRobin(b, kvtest.NewRedis(b))
}
