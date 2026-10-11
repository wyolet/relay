//go:build integration

package session

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestLayout_Redis(t *testing.T) {
	runLayoutSuite(t, func(t *testing.T) kv.Store { return kvtest.NewRedis(t) })
}
