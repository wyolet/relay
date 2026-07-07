package kv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	mrand "math/rand/v2"
	"sort"
	"strconv"
	"strings"
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

const (
	luaAcquire = `
for i, k in ipairs(KEYS) do
  if redis.call('SET', k, ARGV[1], 'NX', 'PX', ARGV[2]) == false then
    for j = 1, i-1 do redis.call('DEL', KEYS[j]) end
    return 0
  end
end
return 1`

	luaRelease = `
local n = 0
for i, k in ipairs(KEYS) do
  if redis.call('GET', k) == ARGV[1] then
    redis.call('DEL', k)
    n = n + 1
  end
end
return n`
)

// WithLock implements the blocking Store contract over SET NX PX: acquisition
// is retried with jittered backoff (5-25ms) until it succeeds or ctx is done,
// matching Mem's block-until-acquired semantics. The all-or-nothing Lua
// acquire (partial holds are rolled back before returning 0) keeps opposite
// key orders deadlock-free while polling.
// Cluster safety: all keys must share the same hash tag, else CROSSSLOT.
func (r *Redis) WithLock(ctx context.Context, keys []string, fn func(context.Context) error) error {
	sorted := make([]string, len(keys))
	copy(sorted, keys)
	sort.Strings(sorted)
	// deduplicate
	deduped := sorted[:0]
	for i, k := range sorted {
		if i == 0 || k != sorted[i-1] {
			deduped = append(deduped, k)
		}
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	token := hex.EncodeToString(tokenBytes)
	ttlMs := strconv.FormatInt(int64(30*time.Second/time.Millisecond), 10)

	for {
		acquired, err := r.runLua(ctx, "state.withlock.acquire", luaAcquire, deduped, token, ttlMs)
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(acquired)) != "0" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(5+mrand.IntN(21)) * time.Millisecond):
		}
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = r.runLua(releaseCtx, "state.withlock.release", luaRelease, deduped, token)
	}()
	return fn(ctx)
}

// runLua is an internal helper; it is NOT the exported RunScript.
// It does SCRIPT LOAD → EVALSHA with EVAL fallback.
func (r *Redis) runLua(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	sha, err := r.loadSHA(ctx, name, script)
	if err != nil {
		return nil, err
	}
	result, err := r.client.EvalSha(ctx, sha, keys, args...).Result()
	if err != nil && strings.Contains(err.Error(), "NOSCRIPT") {
		// Reload and retry.
		r.shas.Delete(name)
		sha, err = r.loadSHA(ctx, name, script)
		if err != nil {
			return nil, err
		}
		result, err = r.client.EvalSha(ctx, sha, keys, args...).Result()
		if err != nil {
			// Final fallback: plain EVAL.
			result, err = r.client.Eval(ctx, script, keys, args...).Result()
		}
	}
	if err != nil {
		return nil, err
	}
	return redisResultToBytes(result)
}

func (r *Redis) loadSHA(ctx context.Context, name, script string) (string, error) {
	if v, ok := r.shas.Load(name); ok {
		return v.(string), nil
	}
	sha, err := r.client.ScriptLoad(ctx, script).Result()
	if err != nil {
		return "", fmt.Errorf("state: SCRIPT LOAD %q: %w", name, err)
	}
	actual, _ := r.shas.LoadOrStore(name, sha)
	return actual.(string), nil
}

// RunScript implements Scripter.
func (r *Redis) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	r.inflight.Add(1)
	defer r.inflight.Done()
	return r.runLua(ctx, name, script, keys, args...)
}

// RunScriptBatch implements BatchScripter: it issues every call in a single
// pipeline (one network round trip on single-node; one per involved node on
// Cluster, dispatched together). Each call is an independent EVALSHA — the
// batch is NOT a transaction, so a per-call failure does not roll back the
// others. Keys within one call must share a hash tag; keys ACROSS calls may
// differ (that is the point — it batches away sequential round trips for
// operations that cannot share a CROSSSLOT-safe script).
func (r *Redis) RunScriptBatch(ctx context.Context, calls []ScriptCall) []ScriptResult {
	r.inflight.Add(1)
	defer r.inflight.Done()

	results := make([]ScriptResult, len(calls))
	if len(calls) == 0 {
		return results
	}

	// Best-effort preload; an empty sha falls back to plain EVAL in-pipeline.
	shas := make([]string, len(calls))
	for i, c := range calls {
		if sha, err := r.loadSHA(ctx, c.Name, c.Script); err == nil {
			shas[i] = sha
		}
	}

	cmds := make([]*redis.Cmd, len(calls))
	// Pipelined returns the first command error; individual results are read
	// from each Cmder below regardless, so the aggregate error is ignored.
	_, _ = r.client.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, c := range calls {
			if shas[i] != "" {
				cmds[i] = p.EvalSha(ctx, shas[i], c.Keys, c.Args...)
			} else {
				cmds[i] = p.Eval(ctx, c.Script, c.Keys, c.Args...)
			}
		}
		return nil
	})

	for i, cmd := range cmds {
		v, err := cmd.Result()
		if err != nil && strings.Contains(err.Error(), "NOSCRIPT") {
			// Script evicted between preload and exec; re-run this one directly
			// (runLua reloads the SHA and falls back to EVAL).
			r.shas.Delete(calls[i].Name)
			b, rerr := r.runLua(ctx, calls[i].Name, calls[i].Script, calls[i].Keys, calls[i].Args...)
			results[i] = ScriptResult{Value: b, Err: rerr}
			continue
		}
		if err != nil {
			results[i] = ScriptResult{Err: err}
			continue
		}
		b, cerr := redisResultToBytes(v)
		results[i] = ScriptResult{Value: b, Err: cerr}
	}
	return results
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

// redisResultToBytes converts a redis Eval result to []byte.
// Integer → decimal string bytes.
// String/[]byte → as-is bytes.
// Slice → JSON-encoded bytes.
func redisResultToBytes(v any) ([]byte, error) {
	switch val := v.(type) {
	case int64:
		return []byte(strconv.FormatInt(val, 10)), nil
	case string:
		return []byte(val), nil
	case []byte:
		return val, nil
	case nil:
		return nil, nil
	default:
		return json.Marshal(val)
	}
}
