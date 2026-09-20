package kv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	mrand "math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"time"
)

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
