package memstore

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	"github.com/wyolet/relay/auth/directory"
)

// table is the rows of one kind keyed by id; stored rows are private copies.
type table[T any] struct {
	rows  map[string]*T
	meta  func(*T) *directory.Metadata
	clone func(*T) *T
}

func newTable[T any](meta func(*T) *directory.Metadata, clone func(*T) *T) *table[T] {
	return &table[T]{rows: map[string]*T{}, meta: meta, clone: clone}
}

// kindStore is the directory.Store of one kind. prepare runs on the caller's row before the write, check on the copy about to be stored, onDelete after the row is gone.
type kindStore[T any] struct {
	s        *Store
	t        *table[T]
	prepare  func(*T)
	check    func(*T) error
	onDelete func(id string)
}

func (k kindStore[T]) Get(_ context.Context, id string) (*T, error) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	r, ok := k.t.rows[id]
	if !ok {
		return nil, nil
	}
	return k.t.clone(r), nil
}

func (k kindStore[T]) List(_ context.Context) ([]*T, error) {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	out := make([]*T, 0, len(k.t.rows))
	for _, r := range k.t.rows {
		out = append(out, k.t.clone(r))
	}
	sort.Slice(out, func(i, j int) bool { return k.t.meta(out[i]).Name < k.t.meta(out[j]).Name })
	return out, nil
}

func (k kindStore[T]) Upsert(_ context.Context, row *T) error {
	if k.prepare != nil {
		k.prepare(row)
	}
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	m := k.t.meta(row)
	want, conditional, err := m.ExpectedVersion()
	if err != nil {
		return err
	}
	stored := k.t.rows[m.ID]
	var version int64 = 1
	if stored != nil {
		have, _ := strconv.ParseInt(k.t.meta(stored).ResourceVersion, 10, 64)
		if conditional && have != want {
			return directory.ErrStaleResourceVersion
		}
		version = have + 1
	}
	for id, r := range k.t.rows {
		if id != m.ID && k.t.meta(r).Name == m.Name {
			return fmt.Errorf("%w: name %q is taken", directory.ErrConflict, m.Name)
		}
	}
	c := k.t.clone(row)
	if k.check != nil {
		if err := k.check(c); err != nil {
			return err
		}
	}
	cm := k.t.meta(c)
	cm.CreatedAt = now()
	if stored != nil {
		cm.CreatedAt = k.t.meta(stored).CreatedAt
	}
	cm.UpdatedAt = now()
	cm.ResourceVersion = directory.FormatResourceVersion(version)
	k.t.rows[m.ID] = c
	return nil
}

func (k kindStore[T]) Delete(_ context.Context, id string) error {
	k.s.mu.Lock()
	defer k.s.mu.Unlock()
	if _, ok := k.t.rows[id]; !ok {
		return nil
	}
	delete(k.t.rows, id)
	if k.onDelete != nil {
		k.onDelete(id)
	}
	return nil
}
