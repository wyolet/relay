package kv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

var ErrLockBusy = errors.New("state: lock busy")

// RedisConfig configures a Redis store.
type RedisConfig struct {
	Addr         string
	Sentinel     *SentinelConfig
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
	client redis.UniversalClient
	shas   sync.Map // name -> sha string
	inflight sync.WaitGroup
}

// NewRedis constructs a Redis and pings the server.
func NewRedis(ctx context.Context, cfg RedisConfig) (*Redis, error) {
	var client redis.UniversalClient
	if cfg.Sentinel != nil {
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

func (r *Redis) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrNotFound
	}
	return v, err
}

func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return r.client.Set(ctx, key, value, ttl).Err()
}

func (r *Redis) Incr(ctx context.Context, key string, delta int64) (int64, error) {
	return r.client.IncrBy(ctx, key, delta).Result()
}

func (r *Redis) Expire(ctx context.Context, key string, ttl time.Duration) error {
	ok, err := r.client.Expire(ctx, key, ttl).Result()
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

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

	acquired, err := r.runLua(ctx, "state.withlock.acquire", luaAcquire, deduped, token, ttlMs)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(acquired)) == "0" {
		return ErrLockBusy
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
