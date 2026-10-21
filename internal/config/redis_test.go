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
