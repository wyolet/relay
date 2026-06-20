package oauth

import (
	"context"
	"sync"
)

// Persister stores a token after it changes — the initial exchange or a
// refresh, which commonly rotates the refresh token. A standalone consumer
// implements it over its own storage (file, DB, keychain) so a process restart,
// and the next refresh, still work. Save runs under the TokenSource's lock and
// should be quick and must not call back into the same TokenSource.
type Persister interface {
	Save(ctx context.Context, tok *Token) error
}

// PersistFunc adapts a plain function to Persister.
type PersistFunc func(ctx context.Context, tok *Token) error

// Save implements Persister.
func (f PersistFunc) Save(ctx context.Context, tok *Token) error { return f(ctx, tok) }

// TokenSource hands out valid access tokens for one credential, refreshing on
// expiry and persisting the (possibly rotated) result via its Persister. It is
// the standalone path — own your token lifecycle in-process, no relay required.
//
// It is safe for concurrent use: refreshes are serialized so only one network
// refresh happens per expiry, and the rotated token is persisted exactly once.
// This is the relay-free counterpart to relay's server-side resolver; both lean
// on the same Flow.Refresh machinery, so neither party is privileged.
type TokenSource struct {
	flow *Flow
	p    Persister

	mu  sync.Mutex
	cur *Token
}

// TokenSource builds a source seeded with the consumer's stored token. Pass a
// Persister to have refreshes (and their refresh-token rotations) written back;
// pass nil for in-memory only (refreshes are lost on restart).
func (f *Flow) TokenSource(tok *Token, p Persister) *TokenSource {
	return &TokenSource{flow: f, p: p, cur: tok}
}

// Token returns a non-expired token, refreshing and persisting first if the
// current one has expired. Concurrent callers share a single refresh.
func (ts *TokenSource) Token(ctx context.Context) (*Token, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	if ts.cur.Valid() {
		return ts.cur, nil
	}
	nt, err := ts.flow.Refresh(ctx, ts.cur)
	if err != nil {
		return nil, err
	}
	if ts.changed(nt) {
		if ts.p != nil {
			if err := ts.p.Save(ctx, nt); err != nil {
				return nil, err
			}
		}
		ts.cur = nt
	}
	return ts.cur, nil
}

// AccessToken is a convenience for callers that only need the bearer string.
func (ts *TokenSource) AccessToken(ctx context.Context) (string, error) {
	t, err := ts.Token(ctx)
	if err != nil {
		return "", err
	}
	return t.AccessToken, nil
}

func (ts *TokenSource) changed(nt *Token) bool {
	return ts.cur == nil ||
		nt.AccessToken != ts.cur.AccessToken ||
		nt.RefreshToken != ts.cur.RefreshToken ||
		!nt.Expiry.Equal(ts.cur.Expiry)
}
