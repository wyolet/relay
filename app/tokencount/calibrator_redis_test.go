//go:build integration

package tokencount

// calibrator_redis_test.go — the same contract against a real Redis. Run with: make test-integration.

import (
	"context"
	"testing"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestCalibrator_Redis(t *testing.T) {
	s, err := kv.NewRedis(context.Background(), kvtest.Config(t))
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	runCalibratorSuite(t, s)
}
