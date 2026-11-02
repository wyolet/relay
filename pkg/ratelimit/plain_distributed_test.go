//go:build integration

package ratelimit

import (
	"testing"

	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestPlainMatchesScript_Redis(t *testing.T) {
	runPlainMatchesScript(t, func(t *testing.T) kvtest.ScriptStore { return kvtest.NewRedis(t) })
}
