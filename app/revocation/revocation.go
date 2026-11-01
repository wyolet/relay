// Package revocation keeps the set of revoked relay-minted tokens. The control plane adds a token id with the token's expiry to one list in the shared store; every pod holds a copy it re-reads on an interval and checks in memory, so the request path makes no store call for it. A revoked token stays denied until its own expiry, when it would fail anyway, and the entry is dropped.
//
// Scope: the list, its in-memory copy and the poll. Out of scope: who may revoke (control API), the bulk sign-out through a user's token version (plans), and the signature check.
//
// Expected kv ops: none per request; one GET per pod per poll; one locked GET and SET per revocation.
package revocation

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// listKey holds every live revocation as one JSON object of token id → expiry (unix seconds). One key keeps the poll at one GET whatever the count; the list is bounded by the revocations made within the longest token lifetime.
const listKey = "{revoked}:tokens"

// Set is the in-memory copy the request path reads.
type Set struct {
	ids atomic.Pointer[map[string]int64]
}

// Revoked reports whether token id is revoked and not yet past its expiry at now.
func (s *Set) Revoked(id string, now time.Time) bool {
	if s == nil || id == "" {
		return false
	}
	m := s.ids.Load()
	if m == nil {
		return false
	}
	exp, ok := (*m)[id]
	return ok && now.Unix() < exp
}

func (s *Set) replace(m map[string]int64) { s.ids.Store(&m) }

// add puts one id into the copy at once, so the pod that revoked never serves the token again.
func (s *Set) add(id string, exp int64) {
	for {
		old := s.ids.Load()
		next := make(map[string]int64, 1)
		if old != nil {
			next = make(map[string]int64, len(*old)+1)
			for k, v := range *old {
				next[k] = v
			}
		}
		next[id] = exp
		if s.ids.CompareAndSwap(old, &next) {
			return
		}
	}
}

// Store is the slice of kv.Store the list uses.
type Store interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	WithLock(ctx context.Context, keys []string, fn func(context.Context) error) error
}

// List is the shared list and this pod's copy of it.
type List struct {
	store Store
	set   Set
	now   func() time.Time
}

// New returns a list over store with an empty copy; Refresh or Run fills it.
func New(store Store) *List { return &List{store: store, now: time.Now} }

// Set returns the copy the request path reads.
func (l *List) Set() *Set { return &l.set }

// Revoke adds token id, valid until exp, to the shared list and to this pod's copy. A token already past exp is not recorded.
func (l *List) Revoke(ctx context.Context, id string, exp time.Time) error {
	now := l.now()
	if !exp.After(now) {
		return nil
	}
	err := l.store.WithLock(ctx, []string{listKey}, func(ctx context.Context) error {
		m, err := l.read(ctx, now)
		if err != nil {
			return err
		}
		m[id] = exp.Unix()
		return l.write(ctx, m, now)
	})
	if err != nil {
		return err
	}
	l.set.add(id, exp.Unix())
	return nil
}

// Refresh replaces this pod's copy with the shared list.
func (l *List) Refresh(ctx context.Context) error {
	m, err := l.read(ctx, l.now())
	if err != nil {
		return err
	}
	l.set.replace(m)
	return nil
}

// Run refreshes the copy every interval until ctx ends. A failed read keeps the previous copy: revocations made meanwhile land on the next good read.
func (l *List) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Refresh(ctx); err != nil {
				slog.Warn("revocation: refresh failed; keeping the previous list", "err", err)
			}
		}
	}
}

// read returns the live entries of the shared list at now; a missing list is empty.
func (l *List) read(ctx context.Context, now time.Time) (map[string]int64, error) {
	raw, err := l.store.Get(ctx, listKey)
	if errors.Is(err, kv.ErrNotFound) || (err == nil && len(raw) == 0) {
		return map[string]int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]int64
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for id, exp := range m {
		if now.Unix() >= exp {
			delete(m, id)
		}
	}
	return m, nil
}

// write stores m with a TTL reaching its latest expiry, so the key goes once every entry has.
func (l *List) write(ctx context.Context, m map[string]int64, now time.Time) error {
	var last int64
	for _, exp := range m {
		last = max(last, exp)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return l.store.Set(ctx, listKey, raw, max(time.Unix(last, 0).Sub(now), time.Second))
}
