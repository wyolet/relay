package kv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisConfig configures a Redis store.
// Exactly one of Addr, Sentinel, or ClusterAddrs should be set.
// When ClusterAddrs is non-empty, a redis.ClusterClient is used and all
// multi-key operations (RunScript, WithLock) require keys to share the
// same hash tag to avoid CROSSSLOT errors.
type RedisConfig struct {
	Addr         string
	Sentinel     *SentinelConfig
	ClusterAddrs []string // non-empty → Cluster mode (redis.NewClusterClient)
	DB           int
	Password     string
	PoolSize     int
	MinIdleConns int
}

// SentinelConfig configures Sentinel-mode failover.
type SentinelConfig struct {
	MasterName       string
	SentinelAddrs    []string
	SentinelPassword string
}

// Redis implements Store (and Scripter) backed by Redis/Valkey.
type Redis struct {
	client   redis.UniversalClient
	shas     sync.Map // name -> sha string
	inflight sync.WaitGroup
}

// NewRedis constructs a Redis and pings the server.
// Precedence: ClusterAddrs > Sentinel > Addr (single-node).
func NewRedis(ctx context.Context, cfg RedisConfig) (*Redis, error) {
	var client redis.UniversalClient
	if len(cfg.ClusterAddrs) > 0 {
		client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:        cfg.ClusterAddrs,
			Password:     cfg.Password,
			PoolSize:     cfg.PoolSize,
			MinIdleConns: cfg.MinIdleConns,
		})
	} else if cfg.Sentinel != nil {
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:       cfg.Sentinel.MasterName,
			SentinelAddrs:    cfg.Sentinel.SentinelAddrs,
			SentinelPassword: cfg.Sentinel.SentinelPassword,
			Password:         cfg.Password,
			DB:               cfg.DB,
			PoolSize:         cfg.PoolSize,
			MinIdleConns:     cfg.MinIdleConns,
		})
	} else {
		client = redis.NewClient(&redis.Options{
			Addr:         cfg.Addr,
			Password:     cfg.Password,
			DB:           cfg.DB,
			PoolSize:     cfg.PoolSize,
			MinIdleConns: cfg.MinIdleConns,
		})
	}
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("state: redis ping: %w", err)
	}
	return &Redis{client: client}, nil
}

// Ping checks the connection.
func (r *Redis) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

// slowOpThreshold is the duration past which a single redis op is logged
// with the client pool's counters. The counters discriminate the failure
// mode a raw "context deadline exceeded" hides: rising pool_timeouts =
// acquire queued behind a saturated pool, rising pool_misses = the op paid
// a fresh dial (TCP + DNS), neither = an established connection stalled
// (server latency or a network-level retransmit stall).
const slowOpThreshold = 500 * time.Millisecond

func (r *Redis) trackSlow(op, key string, start time.Time, err error) {
	d := time.Since(start)
	if d < slowOpThreshold {
		return
	}
	st := r.client.PoolStats()
	slog.Warn("kv: slow redis op",
		"op", op, "key", key, "dur_ms", d.Milliseconds(), "err", err,
		"pool_hits", st.Hits, "pool_misses", st.Misses, "pool_timeouts", st.Timeouts,
		"pool_total", st.TotalConns, "pool_idle", st.IdleConns)
}

func (r *Redis) Get(ctx context.Context, key string) ([]byte, error) {
	start := time.Now()
	v, err := r.client.Get(ctx, key).Bytes()
	r.trackSlow("get", key, start, err)
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	return v, err
}

func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	start := time.Now()
	err := r.client.Set(ctx, key, value, ttl).Err()
	r.trackSlow("set", key, start, err)
	return err
}

func (r *Redis) Del(ctx context.Context, key string) error {
	start := time.Now()
	err := r.client.Del(ctx, key).Err()
	r.trackSlow("del", key, start, err)
	return err
}

func (r *Redis) Incr(ctx context.Context, key string, delta int64) (int64, error) {
	start := time.Now()
	n, err := r.client.IncrBy(ctx, key, delta).Result()
	r.trackSlow("incr", key, start, err)
	return n, err
}

func (r *Redis) Expire(ctx context.Context, key string, ttl time.Duration) error {
	start := time.Now()
	ok, err := r.client.Expire(ctx, key, ttl).Result()
	r.trackSlow("expire", key, start, err)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// TODO(kv): Cluster-unsafe — SCAN only covers one shard.
func (r *Redis) Range(ctx context.Context, prefix string) ([]Entry, error) {
	pattern := prefix + "*"
	var keys []string
	var cursor uint64
	for {
		var batch []string
		var err error
		batch, cursor, err = r.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		keys = append(keys, batch...)
		if cursor == 0 {
			break
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	sort.Strings(keys)

	var entries []Entry
	for i := 0; i < len(keys); i += 100 {
		end := i + 100
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]
		vals, err := r.client.MGet(ctx, batch...).Result()
		if err != nil {
			return nil, err
		}
		for j, v := range vals {
			if v == nil {
				continue
			}
			entries = append(entries, Entry{Key: batch[j], Value: []byte(v.(string))})
		}
	}
	return entries, nil
}

func (r *Redis) Close() error {
	done := make(chan struct{})
	go func() {
		r.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	return r.client.Close()
}
