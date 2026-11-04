//go:build integration

package pipeline

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestRunCallerRefusalMakesNoPick_Redis(t *testing.T) {
	runCallerRefusalMakesNoPick(t, kvtest.NewRedis(t))
}
