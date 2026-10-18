package stored

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/secret/kek"
	"github.com/wyolet/relay/secret/kek/local"
	"github.com/wyolet/relay/secret/keyring"
)

var errNoMasterKey = errors.New("no master key configured (RELAY_MASTER_KEY)")

// keys is what every copy of a Backend shares: the KEK set, the key ring (loaded on first use, so wiring the stores needs no database round trip) and the rewriter's wake-up.
type keys struct {
	pool  *pgxpool.Pool
	set   *kek.Set // nil without a master key
	store *KeyStore

	mu   sync.Mutex
	ring atomic.Pointer[keyring.Ring]
	wake chan struct{}
}

// New builds the stored backend over pool. masterKeys are the KEKs this process holds, RELAY_MASTER_KEY first; none leaves stored secrets unreadable and unwritable, with errors saying so.
func New(pool *pgxpool.Pool, masterKeys [][]byte) (*Backend, error) {
	k := &keys{pool: pool, store: NewKeyStore(pool), wake: make(chan struct{}, 1)}
	if len(masterKeys) > 0 {
		keks := make([]kek.KEK, 0, len(masterKeys))
		for i, b := range masterKeys {
			kk, err := local.New(b)
			if err != nil {
				return nil, fmt.Errorf("secret: master key %d: %w", i+1, err)
			}
			keks = append(keks, kk)
		}
		set, err := kek.NewSet(keks...)
		if err != nil {
			return nil, fmt.Errorf("secret: master keys: %w", err)
		}
		k.set = set
	}
	return &Backend{db: pool, q: gen.New(pool), keys: k}, nil
}

func (k *keys) load(ctx context.Context) (*keyring.Ring, error) {
	if r := k.ring.Load(); r != nil {
		return r, nil
	}
	if k.set == nil {
		return nil, errNoMasterKey
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if r := k.ring.Load(); r != nil {
		return r, nil
	}
	r, err := keyring.New(ctx, k.store, k.set, keyring.Options{
		ReloadError: func(err error) { slog.Warn("secret: data key reload failed", "err", err) },
	})
	if err != nil {
		return nil, fmt.Errorf("load data keys: %w", err)
	}
	k.ring.Store(r)
	return r, nil
}

func (k *keys) wakeRewriter() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// Ring returns the key ring, loading it on first use.
func (s *Backend) Ring(ctx context.Context) (*keyring.Ring, error) {
	return s.keys.load(ctx)
}

// Run loads the key ring, retrying until the database answers, then reloads it periodically and rewrites stale rows until ctx ends. Without a master key it returns at once.
func (s *Backend) Run(ctx context.Context) {
	if s.keys.set == nil {
		return
	}
	var ring *keyring.Ring
	for delay := time.Second; ; delay = min(2*delay, 30*time.Second) {
		r, err := s.keys.load(ctx)
		if err == nil {
			ring = r
			break
		}
		slog.Error("secret: loading data keys failed; retrying", "err", err, "delay", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
	s.warnLegacy(ctx)
	go func() { _ = ring.Run(ctx) }()
	s.keys.wakeRewriter()
	s.runRewriter(ctx, ring)
}

// KeyChanged applies a secretkey change notification: id is a data key's UUID, or "target" for the target KEK. It also wakes the rewriter, since a rotation leaves the scope's rows under a demoted key.
func (s *Backend) KeyChanged(ctx context.Context, id string) error {
	r := s.keys.ring.Load()
	if r == nil {
		return nil
	}
	defer s.keys.wakeRewriter()
	if id == "target" {
		return r.ReloadTarget(ctx)
	}
	var u pgtype.UUID
	if err := u.Scan(id); err != nil {
		return fmt.Errorf("secret: data key id %q: %w", id, err)
	}
	return r.Reload(ctx, u.Bytes)
}

// ReloadKeys re-reads every data key, for a listener that reconnected and may have missed notifications.
func (s *Backend) ReloadKeys(ctx context.Context) error {
	if r := s.keys.ring.Load(); r != nil {
		return r.ReloadAll(ctx)
	}
	return nil
}

func (s *Backend) warnLegacy(ctx context.Context) {
	n, err := s.q.CountLegacySecretValues(ctx)
	if err != nil {
		slog.Warn("secret: counting legacy stored secrets failed", "err", err)
		return
	}
	if n > 0 {
		slog.Warn("secret: stored secrets remain in the legacy master-key shape; the rewriter moves them under data keys", "rows", n)
	}
}
