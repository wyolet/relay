//go:build integration

package keypool

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestPickBatchRedis(t *testing.T) {
	runPickBatchSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}
