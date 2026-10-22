package config

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// Post-flight client defaults. Settling a served request is at most one round trip, so ten connections cover a pod's request rate at sub-millisecond Valkey latency while leaving the server's capacity mostly to admission. A command waits at most pool + write + read (2.5 s with the default write bound), plus one dial on a pool miss, which stays inside the 5 s deadline a served request's post-flight runs under.
const (
	DefaultRedisPostFlightPoolSize    = 10
	DefaultRedisPostFlightPoolTimeout = time.Second
	DefaultRedisPostFlightReadTimeout = time.Second
)

// loadRedisClient reads the go-redis client timeouts and retry count, and the post-flight client's pool and wait bounds.
func loadRedisClient(cfg *Config) error {
	for _, d := range []struct {
		key string
		def time.Duration
		dst *time.Duration
	}{
		{"RELAY_REDIS_DIAL_TIMEOUT_MS", kv.DefaultDialTimeout, &cfg.RedisDialTimeout},
		{"RELAY_REDIS_READ_TIMEOUT_MS", kv.DefaultReadTimeout, &cfg.RedisReadTimeout},
		{"RELAY_REDIS_WRITE_TIMEOUT_MS", kv.DefaultWriteTimeout, &cfg.RedisWriteTimeout},
		{"RELAY_REDIS_POOL_TIMEOUT_MS", kv.DefaultPoolTimeout, &cfg.RedisPoolTimeout},
		{"RELAY_REDIS_POSTFLIGHT_POOL_TIMEOUT_MS", DefaultRedisPostFlightPoolTimeout, &cfg.RedisPostFlightPoolTimeout},
		{"RELAY_REDIS_POSTFLIGHT_READ_TIMEOUT_MS", DefaultRedisPostFlightReadTimeout, &cfg.RedisPostFlightReadTimeout},
	} {
		ms, err := envPositiveInt(d.key, int(d.def/time.Millisecond))
		if err != nil {
			return fmt.Errorf("%s must be >= 1", d.key)
		}
		*d.dst = time.Duration(ms) * time.Millisecond
	}
	if v := envInt("RELAY_REDIS_MAX_RETRIES", 0); v < 0 {
		return fmt.Errorf("RELAY_REDIS_MAX_RETRIES must be >= 0")
	} else {
		cfg.RedisMaxRetries = v
	}
	v, err := envPositiveInt("RELAY_REDIS_POSTFLIGHT_POOL_SIZE", DefaultRedisPostFlightPoolSize)
	if err != nil {
		return fmt.Errorf("RELAY_REDIS_POSTFLIGHT_POOL_SIZE must be >= 1")
	}
	cfg.RedisPostFlightPoolSize = v
	return nil
}
