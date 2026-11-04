//go:build integration

package settle_test

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestQueue_Redis(t *testing.T) {
	runQueueSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}
