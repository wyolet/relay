package config

import (
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

func TestRedisClientDefaults(t *testing.T) {
	for _, k := range []string{"RELAY_REDIS_DIAL_TIMEOUT_MS", "RELAY_REDIS_READ_TIMEOUT_MS", "RELAY_REDIS_WRITE_TIMEOUT_MS", "RELAY_REDIS_POOL_TIMEOUT_MS", "RELAY_REDIS_MAX_RETRIES"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisDialTimeout != kv.DefaultDialTimeout || cfg.RedisReadTimeout != kv.DefaultReadTimeout ||
		cfg.RedisWriteTimeout != kv.DefaultWriteTimeout || cfg.RedisPoolTimeout != kv.DefaultPoolTimeout {
		t.Fatalf("timeouts = dial %v read %v write %v pool %v, want the kv defaults",
			cfg.RedisDialTimeout, cfg.RedisReadTimeout, cfg.RedisWriteTimeout, cfg.RedisPoolTimeout)
	}
	if cfg.RedisMaxRetries != 0 {
		t.Fatalf("RedisMaxRetries = %d, want 0", cfg.RedisMaxRetries)
	}
}

func TestRedisClientFromEnv(t *testing.T) {
	t.Setenv("RELAY_REDIS_DIAL_TIMEOUT_MS", "3000")
	t.Setenv("RELAY_REDIS_READ_TIMEOUT_MS", "750")
	t.Setenv("RELAY_REDIS_WRITE_TIMEOUT_MS", "1000")
	t.Setenv("RELAY_REDIS_POOL_TIMEOUT_MS", "100")
	t.Setenv("RELAY_REDIS_MAX_RETRIES", "2")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisDialTimeout != 3*time.Second || cfg.RedisReadTimeout != 750*time.Millisecond ||
		cfg.RedisWriteTimeout != time.Second || cfg.RedisPoolTimeout != 100*time.Millisecond {
		t.Fatalf("timeouts = dial %v read %v write %v pool %v",
			cfg.RedisDialTimeout, cfg.RedisReadTimeout, cfg.RedisWriteTimeout, cfg.RedisPoolTimeout)
	}
	if cfg.RedisMaxRetries != 2 {
		t.Fatalf("RedisMaxRetries = %d, want 2", cfg.RedisMaxRetries)
	}
}

func TestRedisClientRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"RELAY_REDIS_READ_TIMEOUT_MS", "0"},
		{"RELAY_REDIS_POOL_TIMEOUT_MS", "-1"},
		{"RELAY_REDIS_DIAL_TIMEOUT_MS", "0"},
		{"RELAY_REDIS_WRITE_TIMEOUT_MS", "-5"},
		{"RELAY_REDIS_MAX_RETRIES", "-1"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted %s=%q", tc.key, tc.val)
			}
		})
	}
}

func TestRedisPostFlightDefaults(t *testing.T) {
	for _, k := range []string{"RELAY_REDIS_POSTFLIGHT_POOL_SIZE", "RELAY_REDIS_POSTFLIGHT_POOL_TIMEOUT_MS", "RELAY_REDIS_POSTFLIGHT_READ_TIMEOUT_MS"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisPostFlightPoolSize != 10 || cfg.RedisPostFlightPoolTimeout != time.Second || cfg.RedisPostFlightReadTimeout != time.Second {
		t.Fatalf("post-flight = pool %d, pool wait %v, read %v; want 10, 1s, 1s",
			cfg.RedisPostFlightPoolSize, cfg.RedisPostFlightPoolTimeout, cfg.RedisPostFlightReadTimeout)
	}
}

// A post-flight command waits at most pool + write + read and one dial; on the defaults that fits the 5 s deadline a served request's post-flight runs under, which also keeps a commit's repeats well inside the limiter's idempotency guard.
func TestRedisPostFlightCommandFitsPostFlightDeadline(t *testing.T) {
	command := DefaultRedisPostFlightPoolTimeout + kv.DefaultWriteTimeout + DefaultRedisPostFlightReadTimeout + kv.DefaultDialTimeout
	if command > 5*time.Second {
		t.Fatalf("post-flight command bound = %s, over the 5s post-flight deadline", command)
	}
}

func TestRedisPostFlightFromEnv(t *testing.T) {
	t.Setenv("RELAY_REDIS_POSTFLIGHT_POOL_SIZE", "4")
	t.Setenv("RELAY_REDIS_POSTFLIGHT_POOL_TIMEOUT_MS", "1500")
	t.Setenv("RELAY_REDIS_POSTFLIGHT_READ_TIMEOUT_MS", "800")
	// The admission bounds stay their own.
	t.Setenv("RELAY_REDIS_POOL_TIMEOUT_MS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisPostFlightPoolSize != 4 || cfg.RedisPostFlightPoolTimeout != 1500*time.Millisecond || cfg.RedisPostFlightReadTimeout != 800*time.Millisecond {
		t.Fatalf("post-flight = pool %d, pool wait %v, read %v",
			cfg.RedisPostFlightPoolSize, cfg.RedisPostFlightPoolTimeout, cfg.RedisPostFlightReadTimeout)
	}
	if cfg.RedisPoolTimeout != kv.DefaultPoolTimeout {
		t.Fatalf("RedisPoolTimeout = %v, want the admission default", cfg.RedisPoolTimeout)
	}
}

func TestRedisPostFlightRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"RELAY_REDIS_POSTFLIGHT_POOL_SIZE", "0"},
		{"RELAY_REDIS_POSTFLIGHT_POOL_TIMEOUT_MS", "-1"},
		{"RELAY_REDIS_POSTFLIGHT_READ_TIMEOUT_MS", "0"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted %s=%q", tc.key, tc.val)
			}
		})
	}
}

func TestRedisPasswordFromEnv(t *testing.T) {
	t.Setenv("RELAY_REDIS_ADDR", "valkey:6379")
	t.Setenv("RELAY_REDIS_PASSWORD", "s3cret-valkey-pass")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisPassword != "s3cret-valkey-pass" {
		t.Fatalf("RedisPassword = %q", cfg.RedisPassword)
	}
}
