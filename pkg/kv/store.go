package kv

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("state: key not found")

// Store is the operational-state adapter. M1 ships MemStore; M5 will
// add a Redis-backed implementation.
type Store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Del removes a key. It is a no-op (no error) when the key does not exist.
	Del(ctx context.Context, key string) error
	Incr(ctx context.Context, key string, delta int64) (int64, error)
	Expire(ctx context.Context, key string, ttl time.Duration) error
	Range(ctx context.Context, prefix string) ([]Entry, error)
	// WithLock runs fn while holding an advisory lock over all keys.
	// BLOCKING contract: under contention the call waits (retrying with
	// backoff on distributed backends) until the lock is acquired — every
	// contender eventually runs fn, or returns the ctx error if ctx is
	// cancelled/expires while waiting. It never returns a "busy" error
	// without running fn. Mutual exclusion holds across all keys for the
	// duration of fn.
	WithLock(ctx context.Context, keys []string, fn func(context.Context) error) error
	Close() error
}

type Entry struct {
	Key   string
	Value []byte
}
