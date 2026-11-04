//go:build integration

package keypool

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestPickStateRedis(t *testing.T) {
	runPickStateSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}
