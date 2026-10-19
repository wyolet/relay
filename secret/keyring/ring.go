package keyring

import (
	"context"
	"errors"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wyolet/relay/secret/aead"
	"github.com/wyolet/relay/secret/kek"
)

// Ring holds the unwrapped data keys of every scope and encrypts and decrypts under them. It is safe for concurrent use; Decrypt of a known key takes no lock.
type Ring struct {
	store  Store
	keks   *kek.Set
	legacy []kek.LegacyOpener
	opts   Options

	view atomic.Pointer[view]
	// mu serialises store reads with the view updates they produce, so updates are applied in the order the store answered.
	mu sync.Mutex

	flightMu sync.Mutex
	flights  map[[16]byte]*flight
	misses   map[[16]byte]time.Time // id → until when an unknown or locked key is not read again
}

// view is immutable once published; updates clone it.
type view struct {
	keys    map[[16]byte]*entry
	primary map[string][16]byte
	target  [16]byte
}

type entry struct {
	key DataKey
	dek []byte // nil when locked or destroyed
}

// New loads every key and the target KEK from st and unwraps every live key whose KEK is in keks. Keys it cannot unwrap stay locked; only store errors fail New.
func New(ctx context.Context, st Store, keks *kek.Set, opts Options) (*Ring, error) {
	if st == nil || keks == nil {
		return nil, errors.New("keyring: nil store or KEK set")
	}
	opts, err := opts.withDefaults()
	if err != nil {
		return nil, err
	}
	r := &Ring{
		store:   st,
		keks:    keks,
		opts:    opts,
		flights: map[[16]byte]*flight{},
		misses:  map[[16]byte]time.Time{},
	}
	for _, id := range keks.IDs() {
		k, _ := keks.Get(id)
		if lo, ok := k.(kek.LegacyOpener); ok {
			r.legacy = append(r.legacy, lo)
		}
	}
	r.view.Store(&view{keys: map[[16]byte]*entry{}, primary: map[string][16]byte{}})
	if err := r.ReloadAll(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload re-reads one key from the store: the entry point for change notifications. An id the store no longer has is dropped.
func (r *Ring) Reload(ctx context.Context, id [16]byte) error {
	r.flightMu.Lock()
	delete(r.misses, id)
	r.flightMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reloadIDsLocked(ctx, nil, id)
}

// ReloadTarget re-reads the target KEK.
func (r *Ring) ReloadTarget(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.store.TargetKEK(ctx)
	if err != nil {
		return err
	}
	r.update(func(v *view) { v.target = t })
	return nil
}

// ReloadAll re-reads the target KEK and every key. Keys already unwrapped are kept, not unwrapped again.
func (r *Ring) ReloadAll(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, err := r.store.TargetKEK(ctx)
	if err != nil {
		return err
	}
	all, err := r.store.List(ctx)
	if err != nil {
		return err
	}
	old := r.view.Load()
	nv := &view{keys: make(map[[16]byte]*entry, len(all)), primary: map[string][16]byte{}, target: t}
	for _, k := range all {
		r.apply(ctx, nv, old.keys[k.ID], k)
	}
	r.view.Store(nv)
	return nil
}

// Run reloads every key each Options.RefreshInterval until ctx ends, as the backstop for changes this Ring was never told about. It returns ctx.Err().
func (r *Ring) Run(ctx context.Context) error {
	t := time.NewTicker(r.opts.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			err := r.ReloadAll(ctx)
			switch {
			case err == nil:
				if r.opts.OnReload != nil {
					r.opts.OnReload()
				}
			case ctx.Err() == nil && r.opts.ReloadError != nil:
				r.opts.ReloadError(err)
			}
		}
	}
}

// reloadIDsLocked reads ids (after any keys already in pre) and publishes them in read order, together with any Primary they displace. Caller holds r.mu.
func (r *Ring) reloadIDsLocked(ctx context.Context, pre []DataKey, ids ...[16]byte) error {
	cur := r.view.Load()
	reads := pre
	seen := map[[16]byte]bool{}
	for _, k := range pre {
		seen[k.ID] = true
	}
	var gone [][16]byte
	queue := append([][16]byte(nil), ids...)
	for _, k := range pre {
		queue = append(queue, displaced(cur, k)...)
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		k, err := r.store.Get(ctx, id)
		if errors.Is(err, ErrUnknownKey) {
			gone = append(gone, id)
			continue
		}
		if err != nil {
			return err
		}
		reads = append(reads, k)
		queue = append(queue, displaced(cur, k)...)
	}
	r.update(func(v *view) {
		for _, id := range gone {
			if e := v.keys[id]; e != nil {
				if v.primary[e.key.Scope] == id {
					delete(v.primary, e.key.Scope)
				}
				delete(v.keys, id)
			}
		}
		for _, k := range reads {
			r.apply(ctx, v, v.keys[k.ID], k)
		}
	})
	return nil
}

// displaced returns the Primary the view holds for k's scope when a fresh read shows k as Primary instead; that key was demoted and needs a read of its own.
func displaced(v *view, k DataKey) [][16]byte {
	if k.State != Primary {
		return nil
	}
	if p, ok := v.primary[k.Scope]; ok && p != k.ID {
		return [][16]byte{p}
	}
	return nil
}

// reloadScope re-reads the scope's Primary, the Primary this Ring knew for it, and extra.
func (r *Ring) reloadScope(ctx context.Context, scope string, extra ...[16]byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.view.Load().primary[scope]; ok {
		extra = append(extra, p)
	}
	var pre []DataKey
	k, err := r.store.Primary(ctx, scope)
	switch {
	case err == nil:
		pre = []DataKey{k}
	case !errors.Is(err, ErrUnknownKey):
		return err
	}
	return r.reloadIDsLocked(ctx, pre, extra...)
}

// update publishes a modified copy of the view. Caller holds r.mu.
func (r *Ring) update(fn func(*view)) {
	old := r.view.Load()
	nv := &view{keys: maps.Clone(old.keys), primary: maps.Clone(old.primary), target: old.target}
	fn(nv)
	r.view.Store(nv)
}

// apply puts a freshly read key into v. A DEK already unwrapped is kept as is, together with the scope its unwrap verified; only metadata changes.
func (r *Ring) apply(ctx context.Context, v *view, old *entry, k DataKey) {
	if old != nil && v.primary[old.key.Scope] == k.ID {
		delete(v.primary, old.key.Scope)
	}
	e := &entry{key: k}
	switch {
	case k.State == Destroyed:
		e.key.Wrapped = nil
	case old != nil && old.dek != nil:
		e.dek = old.dek
		e.key.Scope = old.key.Scope
	default:
		e.dek = r.unwrap(ctx, k)
	}
	v.keys[k.ID] = e
	if e.key.State == Primary {
		v.primary[e.key.Scope] = k.ID
	}
}

func (r *Ring) unwrap(ctx context.Context, k DataKey) []byte {
	kk, ok := r.keks.Get(k.KEKID)
	if !ok {
		return nil
	}
	dek, err := kk.Unwrap(ctx, k.Wrapped, wrapAAD(k.ID, k.Scope))
	if err != nil || len(dek) != aead.KeySize {
		return nil
	}
	return dek
}
