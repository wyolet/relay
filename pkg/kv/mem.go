package kv

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type entry struct {
	value    []byte
	deadline time.Time // zero means no expiry
}

func (e entry) expired() bool {
	return !e.deadline.IsZero() && time.Now().After(e.deadline)
}

// Mem is an in-memory Store backed by sync.Map.
type Mem struct {
	data    sync.Map // string -> entry
	mu      sync.Map // string -> *sync.Mutex (per-key locks for WithLock)
	incrMu  sync.Map // string -> *sync.Mutex (per-key locks for Incr atomicity)
	scripts sync.Map // string -> ScriptImpl
	stopCh  chan struct{}
	stopped chan struct{}
}

// RegisterScript registers a Go emulator for a named script.
func (m *Mem) RegisterScript(name string, fn ScriptImpl) {
	m.scripts.Store(name, fn)
}

// RunScript looks up the named emulator and invokes it.
func (m *Mem) RunScript(ctx context.Context, name, _ string, keys []string, args ...any) ([]byte, error) {
	v, ok := m.scripts.Load(name)
	if !ok {
		return nil, fmt.Errorf("state: script %q not registered", name)
	}
	return v.(ScriptImpl)(ctx, m, keys, args)
}

// NewMem constructs a Mem and starts the TTL janitor.
func NewMem() *Mem {
	m := &Mem{
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go m.janitor()
	return m
}

func (m *Mem) janitor() {
	defer close(m.stopped)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.data.Range(func(k, v any) bool {
				if v.(entry).expired() {
					m.data.Delete(k)
				}
				return true
			})
		case <-m.stopCh:
			return
		}
	}
}

func (m *Mem) Get(_ context.Context, key string) ([]byte, error) {
	v, ok := m.data.Load(key)
	if !ok {
		return nil, ErrNotFound
	}
	e := v.(entry)
	if e.expired() {
		m.data.Delete(key)
		return nil, ErrNotFound
	}
	return e.value, nil
}

func (m *Mem) Del(_ context.Context, key string) error {
	m.data.Delete(key)
	return nil
}

func (m *Mem) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	e := entry{value: value}
	if ttl > 0 {
		e.deadline = time.Now().Add(ttl)
	}
	m.data.Store(key, e)
	return nil
}

func (m *Mem) keyMu(store *sync.Map, key string) *sync.Mutex {
	mu := &sync.Mutex{}
	actual, _ := store.LoadOrStore(key, mu)
	return actual.(*sync.Mutex)
}

func (m *Mem) Incr(_ context.Context, key string, delta int64) (int64, error) {
	mu := m.keyMu(&m.incrMu, key)
	mu.Lock()
	defer mu.Unlock()

	var cur int64
	if v, ok := m.data.Load(key); ok {
		e := v.(entry)
		if !e.expired() {
			n, err := strconv.ParseInt(string(e.value), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("state: Incr on non-integer key %q: %w", key, err)
			}
			cur = n
		}
	}
	cur += delta
	// preserve existing deadline
	var deadline time.Time
	if v, ok := m.data.Load(key); ok {
		deadline = v.(entry).deadline
	}
	m.data.Store(key, entry{value: []byte(strconv.FormatInt(cur, 10)), deadline: deadline})
	return cur, nil
}

func (m *Mem) Expire(_ context.Context, key string, ttl time.Duration) error {
	v, ok := m.data.Load(key)
	if !ok {
		return ErrNotFound
	}
	e := v.(entry)
	if e.expired() {
		m.data.Delete(key)
		return ErrNotFound
	}
	if ttl == 0 {
		e.deadline = time.Time{}
	} else {
		e.deadline = time.Now().Add(ttl)
	}
	m.data.Store(key, e)
	return nil
}

func (m *Mem) Range(_ context.Context, prefix string) ([]Entry, error) {
	var entries []Entry
	m.data.Range(func(k, v any) bool {
		key := k.(string)
		if !strings.HasPrefix(key, prefix) {
			return true
		}
		e := v.(entry)
		if e.expired() {
			m.data.Delete(k)
			return true
		}
		entries = append(entries, Entry{Key: key, Value: e.value})
		return true
	})
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Key < entries[j].Key
	})
	return entries, nil
}

func (m *Mem) WithLock(ctx context.Context, keys []string, fn func(context.Context) error) error {
	sorted := make([]string, len(keys))
	copy(sorted, keys)
	sort.Strings(sorted)

	// deduplicate to avoid self-deadlock
	deduped := sorted[:0]
	for i, k := range sorted {
		if i == 0 || k != sorted[i-1] {
			deduped = append(deduped, k)
		}
	}

	mutexes := make([]*sync.Mutex, len(deduped))
	for i, k := range deduped {
		mutexes[i] = m.keyMu(&m.mu, k)
	}
	for _, mu := range mutexes {
		mu.Lock()
	}
	defer func() {
		for i := len(mutexes) - 1; i >= 0; i-- {
			mutexes[i].Unlock()
		}
	}()
	return fn(ctx)
}

// HGet reads one field of a hash stored as synthetic keys "key\x00field".
// Returns ErrNotFound when either the hash key or the field is absent.
func (m *Mem) HGet(_ context.Context, key, field string) ([]byte, error) {
	return m.Get(context.Background(), key+"\x00"+field)
}

// HSet writes one field of a hash stored as synthetic keys "key\x00field".
// When ttl > 0 the field entry gets that TTL; ttl == 0 preserves existing TTL.
func (m *Mem) HSet(_ context.Context, key, field string, value []byte, ttl time.Duration) error {
	fkey := key + "\x00" + field
	if ttl == 0 {
		// preserve deadline if key already exists
		if v, ok := m.data.Load(fkey); ok {
			e := v.(entry)
			if !e.expired() {
				m.data.Store(fkey, entry{value: value, deadline: e.deadline})
				return nil
			}
		}
		m.data.Store(fkey, entry{value: value})
		return nil
	}
	return m.Set(context.Background(), fkey, value, ttl)
}

func (m *Mem) Close() error {
	close(m.stopCh)
	<-m.stopped
	return nil
}
