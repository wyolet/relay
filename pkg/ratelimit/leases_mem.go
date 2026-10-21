package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// leaseGraceMs keeps a lease set alive past its latest lease despite clock skew between pods; reserveLuaScript inlines the same value.
const leaseGraceMs = 1000

// memLeases emulates the concurrency sorted set on kv.Mem: token → lease expiry in ms. Deadline is the key's own expiry, kept so a release can rewrite the value without renewing it, as ZREM leaves the TTL alone.
type memLeases struct {
	Tokens   map[string]int64 `json:"tokens"`
	Deadline time.Time        `json:"deadline"`
}

// live counts the leases still held at nowMs and returns the earliest of their expiries.
func (s memLeases) live(nowMs int64) (held, earliest int64) {
	for _, exp := range s.Tokens {
		if exp <= nowMs {
			continue
		}
		if held == 0 || exp < earliest {
			earliest = exp
		}
		held++
	}
	return held, earliest
}

func (s memLeases) prune(nowMs int64) {
	for tok, exp := range s.Tokens {
		if exp <= nowMs {
			delete(s.Tokens, tok)
		}
	}
}

func memReadLeases(ctx context.Context, store *kv.Mem, key string) (memLeases, error) {
	s := memLeases{Tokens: map[string]int64{}}
	b, err := store.Get(ctx, key)
	if errors.Is(err, kv.ErrNotFound) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("limit: concurrency key holds no lease set: %w", err)
	}
	if s.Tokens == nil {
		s.Tokens = map[string]int64{}
	}
	return s, nil
}

// memWriteLeases stores s for ttl, deleting the key once it is empty or expired, as Redis drops an empty sorted set.
func memWriteLeases(ctx context.Context, store *kv.Mem, key string, s memLeases, ttl time.Duration) error {
	if len(s.Tokens) == 0 || ttl <= 0 {
		return store.Del(ctx, key)
	}
	s.Deadline = time.Now().Add(ttl)
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return store.Set(ctx, key, b, ttl)
}
