package config

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// A post-flight command waits at most pool + write + read and one dial; on the defaults that fits the 5 s deadline a served request's post-flight runs under, which also keeps a commit's repeats well inside the limiter's idempotency guard.
func TestRedisPostFlightCommandFitsPostFlightDeadline(t *testing.T) {
	command := DefaultRedisPostFlightPoolTimeout + kv.DefaultWriteTimeout + DefaultRedisPostFlightReadTimeout + kv.DefaultDialTimeout
	if command > 5*time.Second {
		t.Fatalf("post-flight command bound = %s, over the 5s post-flight deadline", command)
	}
}

// The loaded defaults are the kv client's own, so a relay with no Redis variables behaves as the client documents.
func TestRedisDefaultsFollowTheKVClient(t *testing.T) {
	d := defaults()
	if d.RedisDialTimeout != kv.DefaultDialTimeout || d.RedisReadTimeout != kv.DefaultReadTimeout || d.RedisWriteTimeout != kv.DefaultWriteTimeout || d.RedisPoolTimeout != kv.DefaultPoolTimeout {
		t.Errorf("loaded defaults dial %v read %v write %v pool %v differ from the kv client's", d.RedisDialTimeout, d.RedisReadTimeout, d.RedisWriteTimeout, d.RedisPoolTimeout)
	}
}
