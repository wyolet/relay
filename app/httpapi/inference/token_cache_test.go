package inference

import (
	"crypto/ed25519"
	"net/http"
	"testing"
	"time"

	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/pkg/crypto"
)

func TestVerifiedClaimsAreCachedPerBearer(t *testing.T) {
	f := newPrincipalFixture()
	token := f.mint(t, nil)

	if _, ok := f.tokens.verified(token, time.Now()); !ok {
		t.Fatal("first verification failed")
	}
	// Swap in a key the token was NOT signed with. A cache hit still
	// resolves; a re-verification could not.
	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.tokens.keys.Store(&verifyKeys{current: otherPub, currentKID: crypto.KeyID(otherPub)})
	if _, ok := f.tokens.verified(token, time.Now()); !ok {
		t.Fatal("second verification missed the cache")
	}
}

func TestSetKeyClearsTheClaimsCache(t *testing.T) {
	f := newPrincipalFixture()
	token := f.mint(t, nil)
	if _, ok := f.tokens.verified(token, time.Now()); !ok {
		t.Fatal("first verification failed")
	}

	otherPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Clearing the key set, then installing an unrelated one, leaves nothing
	// this bearer verifies against. A surviving cache entry would still
	// resolve it.
	f.tokens.SetKey(nil)
	f.tokens.SetKey(otherPub)
	if _, ok := f.tokens.verified(token, time.Now()); ok {
		t.Fatal("a bearer signed by a retired key still resolved — the cache was not cleared")
	}
}

func TestExpiredCacheEntryIsNotServed(t *testing.T) {
	f := newPrincipalFixture()
	token := f.mint(t, func(c *crypto.TokenClaims) { c.Exp = time.Now().Add(time.Minute).Unix() })
	now := time.Now()
	if _, ok := f.tokens.verified(token, now); !ok {
		t.Fatal("first verification failed")
	}
	if _, ok := f.tokens.verified(token, now.Add(2*time.Minute)); !ok {
		// A miss is fine — it just re-verifies. What must never happen is
		// the entry being returned as live past its expiry.
		t.Log("expired entry correctly missed and re-verified")
	}
	ent := f.tokens.cache.get(hashToken(token), now.Add(2*time.Minute))
	if ent != nil {
		t.Fatal("cache served an entry past the token's expiry")
	}
}

// A token's subjects must follow the snapshot, not the cache: a user added
// to a group mid-token gets the new subject on the next request.
func TestCachedSubjectsFollowTheSnapshot(t *testing.T) {
	f := newPrincipalFixture()
	f.bindings = []*policybinding.PolicyBinding{
		boundTo(f, "bind-all", 10, f.boundPol.Meta.ID, "group:system:authenticated"),
	}
	token := f.mint(t, nil)
	st := f.stack(t)

	if w := st.do(token); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	before := len(st.seen.Subjects)

	// Publish a new snapshot in which the user is no longer in the group.
	// A fresh value, not a mutation of the fixture's: the snapshot holds
	// that same pointer.
	emptied := *f.group
	emptied.Spec.MemberIDs = []string{meta.NewID()}
	if err := st.cat.ApplyGroupUpsert(&emptied); err != nil {
		t.Fatalf("apply group: %v", err)
	}
	if w := st.do(token); w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	if len(st.seen.Subjects) >= before {
		t.Fatalf("subjects = %v, want fewer than the %d held before the membership change",
			st.seen.Subjects, before)
	}
}

func TestBoundedCacheEvictsWithoutUnboundedGrowth(t *testing.T) {
	f := newPrincipalFixture()
	f.tokens.SetCacheSize(64) // 64/16 shards/2 generations = 2 per generation
	for i := 0; i < 2000; i++ {
		tok := f.mint(t, func(c *crypto.TokenClaims) { c.Jti = meta.NewID() })
		if _, ok := f.tokens.verified(tok, time.Now()); !ok {
			t.Fatalf("verification %d failed", i)
		}
	}
	total := 0
	for i := range f.tokens.cache.shards {
		s := &f.tokens.cache.shards[i]
		s.mu.RLock()
		total += len(s.live) + len(s.previous)
		s.mu.RUnlock()
	}
	if total > 64 {
		t.Fatalf("cache holds %d entries, want at most the configured 64", total)
	}
}

func TestCacheDisabledStillVerifies(t *testing.T) {
	f := newPrincipalFixture()
	f.tokens.SetCacheSize(-1)
	token := f.mint(t, nil)
	if _, ok := f.tokens.verified(token, time.Now()); !ok {
		t.Fatal("verification failed with the cache off")
	}
	if ent := f.tokens.cache.get(hashToken(token), time.Now()); ent != nil {
		t.Fatal("an entry was stored with the cache off")
	}
}
