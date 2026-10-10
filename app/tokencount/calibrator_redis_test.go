//go:build integration

package tokencount

// calibrator_redis_test.go — the same contract against a real Redis. Run with: make test-integration.

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestCalibrator_Redis(t *testing.T) {
	runCalibratorSuite(t, kvtest.NewRedis(t))
}
