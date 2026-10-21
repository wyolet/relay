package config

import (
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// loadRedisClient reads the go-redis client timeouts and retry count.
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
	return nil
}
