package config

import (
	"fmt"
	"runtime"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// Pool sizes scale with GOMAXPROCS, since a pod's request rate and so its concurrent Valkey round trips grow with its CPUs. Under load requests queue for a free connection long before Valkey or relay CPU is busy, so both pools sit well above the round trips a pod has in flight at sub-millisecond latency. A served request makes about one round trip on each (the reserve before the upstream call, the commit after), so they get the same size.
const (
	DefaultRedisPoolSizePerCPU           = 25
	DefaultRedisPostFlightPoolSizePerCPU = 25
)

// Post-flight client wait bounds. A command waits at most pool + write + read (2.5 s with the default write bound), plus one dial on a pool miss, which stays inside the 5 s deadline a served request's post-flight runs under.
const (
	DefaultRedisPostFlightPoolTimeout = time.Second
	DefaultRedisPostFlightReadTimeout = time.Second
)

// Settle queue defaults. A 2 ms wait gathers 10–20 settles per round trip at 5–10k requests a second per pod and delays only post-flight work and a concurrency slot's release. 64 caps one pipeline at some 130–190 commands while catching up after a stall. 4096 queued settles absorb about 400 ms of stalled Valkey at 10k a second before settles go direct.
const (
	DefaultRedisPostFlightBatchSize  = 64
	DefaultRedisPostFlightBatchDelay = 2 * time.Millisecond
	DefaultRedisPostFlightQueueSize  = 4096
)

// loadRedisClient reads the go-redis client timeouts, retry count and admission wait bound, and the post-flight client's pool, wait bounds and settle queue. It runs after RedisPoolSize is read.
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
	waiters, err := envPositiveInt("RELAY_REDIS_POOL_MAX_WAITERS", cfg.RedisPoolSize)
	if err != nil {
		return fmt.Errorf("RELAY_REDIS_POOL_MAX_WAITERS must be >= 1")
	}
	cfg.RedisPoolMaxWaiters = waiters
	v, err := envPositiveInt("RELAY_REDIS_POSTFLIGHT_POOL_SIZE", DefaultRedisPostFlightPoolSizePerCPU*runtime.GOMAXPROCS(0))
	if err != nil {
		return fmt.Errorf("RELAY_REDIS_POSTFLIGHT_POOL_SIZE must be >= 1")
	}
	cfg.RedisPostFlightPoolSize = v
	return loadSettleQueue(cfg)
}

func loadSettleQueue(cfg *Config) error {
	size, err := envPositiveInt("RELAY_REDIS_POSTFLIGHT_BATCH_SIZE", DefaultRedisPostFlightBatchSize)
	if err != nil {
		return fmt.Errorf("RELAY_REDIS_POSTFLIGHT_BATCH_SIZE must be >= 1")
	}
	cfg.RedisPostFlightBatchSize = size
	delayMs := envInt("RELAY_REDIS_POSTFLIGHT_BATCH_DELAY_MS", int(DefaultRedisPostFlightBatchDelay/time.Millisecond))
	if delayMs < 0 {
		return fmt.Errorf("RELAY_REDIS_POSTFLIGHT_BATCH_DELAY_MS must be >= 0")
	}
	cfg.RedisPostFlightBatchDelay = time.Duration(delayMs) * time.Millisecond
	queue := envInt("RELAY_REDIS_POSTFLIGHT_QUEUE_SIZE", DefaultRedisPostFlightQueueSize)
	if queue < 0 {
		return fmt.Errorf("RELAY_REDIS_POSTFLIGHT_QUEUE_SIZE must be >= 0")
	}
	cfg.RedisPostFlightQueueSize = queue
	return nil
}
