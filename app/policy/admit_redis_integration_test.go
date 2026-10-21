//go:build integration

package policy

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestAdmitRedis(t *testing.T) {
	runAdmitSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}

func TestSettleRedis(t *testing.T) {
	runSettleSuite(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}
