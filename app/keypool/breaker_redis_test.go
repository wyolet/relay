//go:build integration

package keypool

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestBreakerRedis(t *testing.T) {
	runBreakerSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}
