package kv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
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

	// Zero durations take the Default* values below.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolTimeout  time.Duration
	// MaxRetries is how many times a failed command is resent; 0 sends it once.
	MaxRetries int

	// Pool names this client in the slow-op log, so a process with more than one pool shows which one ran out.
	Pool string
}

// Client defaults: short enough that a stalled server costs a request about a second instead of the library's multi-second defaults. The read bound outlasts two TCP retransmit timeouts (~420–600 ms), so one lost packet doesn't close the connection and force a redial.
const (
	DefaultDialTimeout  = 2 * time.Second
	DefaultReadTimeout  = 1 * time.Second
	DefaultWriteTimeout = 500 * time.Millisecond
	DefaultPoolTimeout  = 250 * time.Millisecond
)

// ErrUnavailable marks a call the store could not serve: no reply in time or at all (timeout, exhausted pool, refused or dropped connection), or a reply that the server cannot serve right now (LOADING, READONLY, BUSY, ...). Other error replies are the command's own failure and stay unwrapped. Whether the command ran on the server is unknown.
var ErrUnavailable = errors.New("state: store unavailable")

func (c *RedisConfig) withDefaults() RedisConfig {
	out := *c
	if out.DialTimeout <= 0 {
		out.DialTimeout = DefaultDialTimeout
	}
	if out.ReadTimeout <= 0 {
		out.ReadTimeout = DefaultReadTimeout
	}
	if out.WriteTimeout <= 0 {
		out.WriteTimeout = DefaultWriteTimeout
	}
	if out.PoolTimeout <= 0 {
		out.PoolTimeout = DefaultPoolTimeout
	}
	return out
}

// goRedisRetries maps a retry count to go-redis, where 0 means its default of 3 and -1 means none.
func goRedisRetries(n int) int {
	if n <= 0 {
		return -1
	}
	return n
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
	pool     string
	shas     sync.Map // name -> sha string
	inflight sync.WaitGroup
}

// NewRedis constructs a Redis and pings the server.
// Precedence: ClusterAddrs > Sentinel > Addr (single-node).
//
// Each command sent is bounded by PoolTimeout + WriteTimeout + ReadTimeout, plus one DialTimeout on a pool miss: the caller waits on the dial itself, which PoolTimeout does not cover, so dial retries are off. A script call sends one EVALSHA when its SHA is cached, but a SCRIPT LOAD on a cold cache and the NOSCRIPT reload (SCRIPT LOAD, EVALSHA, then EVAL) add commands, up to five bounds in all. The cluster client also resends through its redirect loop (MaxRedirects), which MaxRetries does not govern, so there a script can run twice.
func NewRedis(ctx context.Context, cfg RedisConfig) (*Redis, error) {
	cfg = cfg.withDefaults()
	retries := goRedisRetries(cfg.MaxRetries)
	var client redis.UniversalClient
	if len(cfg.ClusterAddrs) > 0 {
		client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:                 cfg.ClusterAddrs,
			Password:              cfg.Password,
			PoolSize:              cfg.PoolSize,
			MinIdleConns:          cfg.MinIdleConns,
			DialTimeout:           cfg.DialTimeout,
			DialerRetries:         1,
			ReadTimeout:           cfg.ReadTimeout,
			WriteTimeout:          cfg.WriteTimeout,
			PoolTimeout:           cfg.PoolTimeout,
			MaxRetries:            retries,
			ContextTimeoutEnabled: true,
		})
	} else if cfg.Sentinel != nil {
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:            cfg.Sentinel.MasterName,
			SentinelAddrs:         cfg.Sentinel.SentinelAddrs,
			SentinelPassword:      cfg.Sentinel.SentinelPassword,
			Password:              cfg.Password,
			DB:                    cfg.DB,
			PoolSize:              cfg.PoolSize,
			MinIdleConns:          cfg.MinIdleConns,
			DialTimeout:           cfg.DialTimeout,
			DialerRetries:         1,
			ReadTimeout:           cfg.ReadTimeout,
			WriteTimeout:          cfg.WriteTimeout,
			PoolTimeout:           cfg.PoolTimeout,
			MaxRetries:            retries,
			ContextTimeoutEnabled: true,
		})
	} else {
		client = redis.NewClient(&redis.Options{
			Addr:                  cfg.Addr,
			Password:              cfg.Password,
			DB:                    cfg.DB,
			PoolSize:              cfg.PoolSize,
			MinIdleConns:          cfg.MinIdleConns,
			DialTimeout:           cfg.DialTimeout,
			DialerRetries:         1,
			ReadTimeout:           cfg.ReadTimeout,
			WriteTimeout:          cfg.WriteTimeout,
			PoolTimeout:           cfg.PoolTimeout,
			MaxRetries:            retries,
			ContextTimeoutEnabled: true,
		})
	}
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("state: redis ping: %w", err)
	}
	return &Redis{client: client, pool: cfg.Pool}, nil
}

// isUnavailable reports whether err is an ErrUnavailable case. A cancelled caller is not: that is the caller leaving, not the store failing.
func isUnavailable(err error) bool {
	if err == nil || errors.Is(err, redis.Nil) || errors.Is(err, context.Canceled) {
		return false
	}
	var reply redis.Error
	return !errors.As(err, &reply) || serverCannotServe(err)
}

func markUnavailable(err error) error {
	if !isUnavailable(err) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

func serverCannotServe(err error) bool {
	return redis.IsLoadingError(err) || redis.IsReadOnlyError(err) || redis.IsMasterDownError(err) ||
		redis.IsClusterDownError(err) || redis.IsTryAgainError(err) || redis.IsMaxClientsError(err) ||
		redis.IsOOMError(err) || redis.HasErrorPrefix(err, "BUSY")
}

// Ping checks the connection.
func (r *Redis) Ping(ctx context.Context) error {
	return markUnavailable(r.client.Ping(ctx).Err())
}

// slowOpThreshold is the duration past which a single redis op is logged
// with the client pool's counters. The counters discriminate the failure
// mode a raw "context deadline exceeded" hides: rising pool_timeouts =
// acquire queued behind a saturated pool, rising pool_misses = the op paid
// a fresh dial (TCP + DNS), neither = an established connection stalled
// (server latency or a network-level retransmit stall).
const slowOpThreshold = 500 * time.Millisecond

// slowLogInterval spaces the slow-op WARN process-wide: during an outage every call times out, and a synchronous line per call would cost a saturated pod more than the call.
const slowLogInterval = time.Second

var (
	slowSinceLog     atomic.Int64
	slowLastLogNanos atomic.Int64
)

// trackSlow counts an op that was slow or failed for want of the store, and logs at most one line per slowLogInterval carrying the count since the previous line and the latest op.
func (r *Redis) trackSlow(op, key string, start time.Time, err error) {
	d := time.Since(start)
	if d < slowOpThreshold && !isUnavailable(err) {
		return
	}
	slowSinceLog.Add(1)
	now := time.Now().UnixNano()
	last := slowLastLogNanos.Load()
	if now-last < int64(slowLogInterval) || !slowLastLogNanos.CompareAndSwap(last, now) {
		return
	}
	st := r.client.PoolStats()
	slog.Warn("kv: slow or failed redis ops",
		"ops", slowSinceLog.Swap(0), "pool", r.pool,
		"op", op, "key", redactKey(key), "dur_ms", d.Milliseconds(), "err", err,
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
	return v, markUnavailable(err)
}

func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	start := time.Now()
	err := r.client.Set(ctx, key, value, ttl).Err()
	r.trackSlow("set", key, start, err)
	return markUnavailable(err)
}

func (r *Redis) Del(ctx context.Context, key string) error {
	start := time.Now()
	err := r.client.Del(ctx, key).Err()
	r.trackSlow("del", key, start, err)
	return markUnavailable(err)
}

func (r *Redis) Incr(ctx context.Context, key string, delta int64) (int64, error) {
	start := time.Now()
	n, err := r.client.IncrBy(ctx, key, delta).Result()
	r.trackSlow("incr", key, start, err)
	return n, markUnavailable(err)
}

func (r *Redis) Expire(ctx context.Context, key string, ttl time.Duration) error {
	start := time.Now()
	ok, err := r.client.Expire(ctx, key, ttl).Result()
	r.trackSlow("expire", key, start, err)
	if err != nil {
		return markUnavailable(err)
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
			return nil, markUnavailable(err)
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
			return nil, markUnavailable(err)
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
