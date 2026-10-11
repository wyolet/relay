package token

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults for KeySetOptions' zero values.
const (
	DefaultRefreshInterval   = time.Minute
	DefaultMinReloadInterval = 10 * time.Second
	DefaultLoadTimeout       = 2 * time.Second
)

// ErrNoSigningKey means the key set holds no signing key. ErrUnknownKey means
// no held key matches the token's kid, even after any reload the rate limit
// allows; it wraps ErrSignature.
var (
	ErrNoSigningKey = errors.New("token: no signing key")
	ErrUnknownKey   = fmt.Errorf("%w: no key for the token's kid", ErrSignature)
)

// KeySetOptions tune how a KeySet follows its store.
type KeySetOptions struct {
	// RefreshInterval is how often Run reloads the store.
	RefreshInterval time.Duration
	// MinReloadInterval is the least time between loads an unknown kid may
	// trigger, so a caller spraying random kids costs at most one load per
	// interval. Negative turns those loads off, for a store only this process
	// writes.
	MinReloadInterval time.Duration
	// LoadTimeout bounds a load an unknown kid triggers. That load outlives
	// the request that triggered it, since other requests may be waiting on it.
	LoadTimeout time.Duration
	// OnError receives the errors of loads no caller waits on: Run's, and
	// those an unknown kid triggers.
	OnError func(error)
}

// KeySet is the current signing key plus the retired keys that still verify,
// read from a KeyStore. A lookup reads memory only, except that an unknown kid
// may trigger one single-flighted, rate-limited reload.
type KeySet struct {
	store KeyStore
	opts  KeySetOptions
	table atomic.Pointer[keyTable]

	// loadMu serializes read-and-install, so a slow load can never install
	// keys older than a load that finished after it started.
	loadMu sync.Mutex

	mu       sync.Mutex // guards lastLoad and inflight
	lastLoad time.Time
	inflight chan struct{}
}

// NewKeySet returns a key set over store. It holds no keys until the first
// Reload.
func NewKeySet(store KeyStore, opts KeySetOptions) *KeySet {
	if opts.RefreshInterval <= 0 {
		opts.RefreshInterval = DefaultRefreshInterval
	}
	if opts.MinReloadInterval == 0 {
		opts.MinReloadInterval = DefaultMinReloadInterval
	}
	if opts.LoadTimeout <= 0 {
		opts.LoadTimeout = DefaultLoadTimeout
	}
	return &KeySet{store: store, opts: opts}
}

// Reload reads the store and installs what it holds. On error the keys held
// before stay installed.
func (s *KeySet) Reload(ctx context.Context) error {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	s.mu.Lock()
	s.lastLoad = time.Now()
	s.mu.Unlock()
	k, err := s.store.LoadKeys(ctx)
	if err != nil {
		return fmt.Errorf("token: load keys: %w", err)
	}
	t, err := newKeyTable(k)
	if err != nil {
		return err
	}
	s.table.Store(t)
	return nil
}

// Run reloads the store every RefreshInterval until ctx is done. Call Reload
// once first: Run does not load at start.
func (s *KeySet) Run(ctx context.Context) {
	tick := time.NewTicker(s.opts.RefreshInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.report(s.Reload(ctx))
		}
	}
}

// Keys returns the generation installed now.
func (s *KeySet) Keys() Keys {
	if t := s.table.Load(); t != nil {
		return t.keys
	}
	return Keys{}
}

// Current returns the key new tokens are minted under, or nil.
func (s *KeySet) Current() ed25519.PublicKey {
	if t := s.table.Load(); t != nil {
		return t.keys.Current
	}
	return nil
}

// PublicKeys returns every key that verifies, the current one first.
func (s *KeySet) PublicKeys() []ed25519.PublicKey {
	t := s.table.Load()
	if t == nil {
		return nil
	}
	out := make([]ed25519.PublicKey, len(t.entries))
	for i, e := range t.entries {
		out[i] = e.pub
	}
	return out
}

// JWKSet returns the verification keys as the document a `jwks_uri` serves.
func (s *KeySet) JWKSet() (PublicJWKSet, error) {
	return JWKSet(s.PublicKeys()...)
}

// Sign mints a token for claims under the signing key, naming it by kid.
func (s *KeySet) Sign(claims any) (string, error) {
	t := s.table.Load()
	if t == nil || t.keys.Signing == nil {
		return "", ErrNoSigningKey
	}
	return Sign(t.keys.Signing, t.entries[0].kid, claims)
}

// PublicKey returns the key kid names; "" names the current key. A miss may
// reload the store once, within the rate limit, waiting for the load or ctx.
func (s *KeySet) PublicKey(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	if pub, ok := s.table.Load().lookup(kid); ok {
		return pub, nil
	}
	if !s.reloadAfterMiss(ctx) {
		return nil, ErrUnknownKey
	}
	if pub, ok := s.table.Load().lookup(kid); ok {
		return pub, nil
	}
	return nil, ErrUnknownKey
}

// Verify picks the key the token's kid names in s and parses the token with
// it. Claim policy is the caller's; see Validate.
func Verify[C any](ctx context.Context, s *KeySet, token string) (C, error) {
	pub, err := s.PublicKey(ctx, HeaderKeyID(token))
	if err != nil {
		var zero C
		return zero, err
	}
	return Parse[C](pub, token)
}

// reloadAfterMiss runs or joins the one load an unknown kid may trigger,
// reporting whether a load completed. It reports false at once while the
// rate limit holds.
func (s *KeySet) reloadAfterMiss(ctx context.Context) bool {
	if s.opts.MinReloadInterval < 0 {
		return false
	}
	s.mu.Lock()
	if done := s.inflight; done != nil {
		s.mu.Unlock()
		select {
		case <-done:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if !s.lastLoad.IsZero() && time.Since(s.lastLoad) < s.opts.MinReloadInterval {
		s.mu.Unlock()
		return false
	}
	done := make(chan struct{})
	s.inflight = done
	s.mu.Unlock()
	// Deferred so a panicking store cannot leave every later miss waiting.
	defer func() {
		s.mu.Lock()
		s.inflight = nil
		s.mu.Unlock()
		close(done)
	}()

	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.LoadTimeout)
	defer cancel()
	s.report(s.Reload(lctx))
	return true
}

func (s *KeySet) report(err error) {
	if err != nil && s.opts.OnError != nil {
		s.opts.OnError(err)
	}
}
