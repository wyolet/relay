//go:build integration

package pipeline

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestRunCallerRefusalMakesNoPick_Redis(t *testing.T) {
	runCallerRefusalMakesNoPick(t, kvtest.NewRedis(t))
}

func TestRunWarmMakesNoStoreCall_Redis(t *testing.T) {
	runWarmMakesNoStoreCall(t, kvtest.NewRedis(t))
}
