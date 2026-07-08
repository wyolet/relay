package keypool

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// recordSuccessScript atomically records a key as healthy. The GET+SET runs
// server-side in one round trip so a concurrent RecordFailure on another pod
// cannot interleave between our read and write (the lost-update race the old
// non-atomic Go-side GET+SET exposed). The prior raw record is returned so the
// caller can log the from-state transition; "" means no prior record existed.
//
// KEYS[1] = circuit key
// ARGV[1] = new record JSON (pre-marshalled by the caller)
// ARGV[2] = ttl_ms (> 0 → PX expiry; 0 → persist)
const recordSuccessScript = `
local old = redis.call('GET', KEYS[1])
local ttl = tonumber(ARGV[2])
if ttl and ttl > 0 then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ttl)
else
  redis.call('SET', KEYS[1], ARGV[1])
end
if old == false then return '' end
return old
`

const scriptRecordSuccess = "keypool.record_success"

// RegisterScripts installs the Go emulators for keypool's Lua scripts on a
// MemStore, mirroring the Redis-side behaviour exactly. New registers them
// automatically when the store IS a *kv.Mem; test doubles that merely embed
// one must call this on the inner Mem themselves (same convention as
// pkgratelimit.RegisterScripts).
func RegisterScripts(m *kv.Mem) {
	m.RegisterScript(scriptRecordSuccess, memRecordSuccessImpl)
}

// memRecordSuccessImpl is the Mem emulator for recordSuccessScript.
func memRecordSuccessImpl(ctx context.Context, store *kv.Mem, keys []string, args []any) ([]byte, error) {
	if len(keys) < 1 {
		return nil, fmt.Errorf("keypool.record_success: expected 1 key, got %d", len(keys))
	}
	if len(args) < 2 {
		return nil, fmt.Errorf("keypool.record_success: expected 2 args, got %d", len(args))
	}
	newVal, ok := args[0].(string)
	if !ok {
		return nil, fmt.Errorf("keypool.record_success: arg[0] must be string")
	}
	ttlMs, err := toInt64(args[1])
	if err != nil {
		return nil, fmt.Errorf("keypool.record_success: arg[1] ttl_ms: %w", err)
	}

	key := keys[0]

	var old []byte
	lockErr := store.WithLock(ctx, keys, func(ctx context.Context) error {
		if b, gerr := store.Get(ctx, key); gerr == nil {
			old = append([]byte(nil), b...)
		}
		ttl := time.Duration(ttlMs) * time.Millisecond
		return store.Set(ctx, key, []byte(newVal), ttl)
	})
	if lockErr != nil {
		return nil, lockErr
	}
	return old, nil
}

func toInt64(v any) (int64, error) {
	switch x := v.(type) {
	case int64:
		return x, nil
	case int:
		return int64(x), nil
	case float64:
		return int64(x), nil
	case string:
		return strconv.ParseInt(x, 10, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to int64", v)
	}
}
