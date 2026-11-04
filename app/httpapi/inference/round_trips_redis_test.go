//go:build integration

package inference

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestPlainRequestRoundTrips_Redis(t *testing.T) {
	runPlainRequestRoundTrips(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}
