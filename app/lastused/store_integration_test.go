//go:build integration

// store_integration_test.go covers the last-used writes against a real
// Postgres: the later time wins, deleted rows are skipped, and the triggers
// on relay_keys and secrets ignore a change to last_used_at alone while every
// other write still notifies and bumps the version. Run with: make test-integration.

package lastused_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/lastused"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/meta"
	"github.com/wyolet/relay/secret"
)

type stores struct {
	keys     *key.Store
	hostKeys *hostkey.Store
	touch    *lastused.Store
}

func newStores(pool *pgxpool.Pool) stores {
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	return stores{key.NewStore(gen.New(pool)), hostkey.NewStore(pool, reg, nil), lastused.NewStore(gen.New(pool))}
}

// seedCredentials writes n relay keys and n host keys, returning their ids.
func seedCredentials(t *testing.T, pool *pgxpool.Pool, n int) (keyIDs, hostKeyIDs []string) {
	t.Helper()
	ctx := context.Background()
	u := &user.User{ID: meta.NewID(), Username: "lastused-" + meta.NewID()[:8]}
	if err := user.NewStore(pool).Upsert(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	s := newStores(pool)
	for i := range n {
		id := meta.NewID()
		k := &key.Key{Meta: meta.Metadata{ID: id, Name: "key-" + id[len(id)-12:], Owner: meta.Owner{Kind: meta.OwnerUser, ID: u.ID}}}
		k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: u.ID}
		k.Spec.KeyHash = fmt.Sprintf("%064x", i+1)
		if err := s.keys.Upsert(ctx, k); err != nil {
			t.Fatalf("seed key: %v", err)
		}
		keyIDs = append(keyIDs, id)

		hid := meta.NewID()
		hk := &hostkey.HostKey{
			Meta: meta.Metadata{ID: hid, Name: "hk-" + hid[len(hid)-12:], Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: hostkey.Spec{HostID: meta.NewID(), AccountID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "LASTUSED_TEST_KEY"}},
		}
		if err := s.hostKeys.Upsert(ctx, hk); err != nil {
			t.Fatalf("seed host key: %v", err)
		}
		hostKeyIDs = append(hostKeyIDs, hid)
	}
	return keyIDs, hostKeyIDs
}

func entries(ids []string, at time.Time) []lastused.Entry {
	out := make([]lastused.Entry, len(ids))
	for i, id := range ids {
		out[i] = lastused.Entry{ID: id, At: at}
	}
	return out
}

func getKey(t *testing.T, s stores, id string) *key.Key {
	t.Helper()
	k, err := s.keys.Get(context.Background(), id)
	if err != nil || k == nil {
		t.Fatalf("get key %s: %v, %v", id, k, err)
	}
	return k
}

func getHostKey(t *testing.T, s stores, id string) *hostkey.HostKey {
	t.Helper()
	k, err := s.hostKeys.Get(context.Background(), id)
	if err != nil || k == nil {
		t.Fatalf("get host key %s: %v, %v", id, k, err)
	}
	return k
}

func sameTime(got *time.Time, want time.Time) bool {
	return got != nil && got.Equal(want)
}

// listen opens a connection listening on catalog_events.
func listen(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *pgxpool.Conn {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Release)
	if _, err := conn.Exec(ctx, "LISTEN catalog_events"); err != nil {
		t.Fatal(err)
	}
	return conn
}

// notified drains catalog_events for wait and reports whether any payload named id.
func notified(ctx context.Context, conn *pgxpool.Conn, id string, wait time.Duration) bool {
	deadline, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	seen := false
	for {
		n, err := conn.Conn().WaitForNotification(deadline)
		if err != nil {
			return seen
		}
		if strings.HasSuffix(n.Payload, ":"+id) {
			seen = true
		}
	}
}

func TestIntegration_TouchKeepsTheLaterTime(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	keyIDs, hostKeyIDs := seedCredentials(t, pool, 2)
	s := newStores(pool)
	early := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	late := early.Add(5 * time.Minute)

	if err := s.touch.TouchKeys(ctx, entries(keyIDs[:1], late)); err != nil {
		t.Fatal(err)
	}
	// A pod with an older time writes after: the stored time stays.
	if err := s.touch.TouchKeys(ctx, entries(keyIDs, early)); err != nil {
		t.Fatal(err)
	}
	if err := s.touch.TouchHostKeys(ctx, entries(hostKeyIDs[1:], early)); err != nil {
		t.Fatal(err)
	}
	if got := getKey(t, s, keyIDs[0]).Status.LastUsedAt; !sameTime(got, late) {
		t.Errorf("key 0 lastUsedAt = %v, want %v", got, late)
	}
	if got := getKey(t, s, keyIDs[1]).Status.LastUsedAt; !sameTime(got, early) {
		t.Errorf("key 1 lastUsedAt = %v, want %v", got, early)
	}
	if got := getHostKey(t, s, hostKeyIDs[0]).Status.LastUsedAt; got != nil {
		t.Errorf("unused host key lastUsedAt = %v, want none", got)
	}
	if got := getHostKey(t, s, hostKeyIDs[1]).Status.LastUsedAt; !sameTime(got, early) {
		t.Errorf("host key 1 lastUsedAt = %v, want %v", got, early)
	}
	keys, err := s.keys.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		if k.Meta.ID == keyIDs[0] && !sameTime(k.Status.LastUsedAt, late) {
			t.Errorf("listed key lastUsedAt = %v, want %v", k.Status.LastUsedAt, late)
		}
	}
}

// Ids of keys deleted after they served match no row.
func TestIntegration_TouchSkipsDeletedKeys(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	keyIDs, hostKeyIDs := seedCredentials(t, pool, 1)
	s := newStores(pool)
	at := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	gone := meta.NewID()
	if err := s.touch.TouchKeys(ctx, entries([]string{keyIDs[0], gone}, at)); err != nil {
		t.Fatalf("TouchKeys with a deleted id: %v", err)
	}
	if err := s.touch.TouchHostKeys(ctx, entries([]string{gone, hostKeyIDs[0]}, at)); err != nil {
		t.Fatalf("TouchHostKeys with a deleted id: %v", err)
	}
	if got := getKey(t, s, keyIDs[0]).Status.LastUsedAt; !sameTime(got, at) {
		t.Errorf("key lastUsedAt = %v, want %v", got, at)
	}
	if got := getHostKey(t, s, hostKeyIDs[0]).Status.LastUsedAt; !sameTime(got, at) {
		t.Errorf("host key lastUsedAt = %v, want %v", got, at)
	}
}

// Two pods flushing the same rows at once, each its own times: every row ends at the later one.
func TestIntegration_ConcurrentFlushersKeepTheLaterTime(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	keyIDs, hostKeyIDs := seedCredentials(t, pool, 20)
	s := newStores(pool)
	early := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	late := early.Add(time.Minute)
	for range 10 {
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for _, at := range []time.Time{early, late} {
			wg.Go(func() { errs <- s.touch.TouchKeys(ctx, entries(keyIDs, at)) })
			wg.Go(func() { errs <- s.touch.TouchHostKeys(ctx, entries(hostKeyIDs, at)) })
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range keyIDs {
		if got := getKey(t, s, keyIDs[i]).Status.LastUsedAt; !sameTime(got, late) {
			t.Errorf("key %s = %v, want %v", keyIDs[i], got, late)
		}
		if got := getHostKey(t, s, hostKeyIDs[i]).Status.LastUsedAt; !sameTime(got, late) {
			t.Errorf("host key %s = %v, want %v", hostKeyIDs[i], got, late)
		}
	}
}

// The writes land once a minute from every pod: they must neither notify pods to rebuild the catalog nor make an operator's copy stale.
func TestIntegration_TouchNeitherNotifiesNorBumpsVersion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := storagetest.Pool(t)
	keyIDs, hostKeyIDs := seedCredentials(t, pool, 1)
	s := newStores(pool)
	k0, hk0 := getKey(t, s, keyIDs[0]), getHostKey(t, s, hostKeyIDs[0])
	conn := listen(t, ctx, pool)
	at := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	for _, a := range []time.Time{at, at.Add(time.Minute)} {
		if err := s.touch.TouchKeys(ctx, entries(keyIDs, a)); err != nil {
			t.Fatal(err)
		}
		if err := s.touch.TouchHostKeys(ctx, entries(hostKeyIDs, a)); err != nil {
			t.Fatal(err)
		}
	}
	wait, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	if n, err := conn.Conn().WaitForNotification(wait); err == nil {
		t.Errorf("notification %q on %s after last-used writes, want none", n.Payload, n.Channel)
	}
	k1, hk1 := getKey(t, s, keyIDs[0]), getHostKey(t, s, hostKeyIDs[0])
	if k1.Meta.ResourceVersion != k0.Meta.ResourceVersion || !k1.Meta.UpdatedAt.Equal(k0.Meta.UpdatedAt) {
		t.Errorf("key version %s at %v, want %s at %v", k1.Meta.ResourceVersion, k1.Meta.UpdatedAt, k0.Meta.ResourceVersion, k0.Meta.UpdatedAt)
	}
	if hk1.Meta.ResourceVersion != hk0.Meta.ResourceVersion || !hk1.Meta.UpdatedAt.Equal(hk0.Meta.UpdatedAt) {
		t.Errorf("host key version %s at %v, want %s at %v", hk1.Meta.ResourceVersion, hk1.Meta.UpdatedAt, hk0.Meta.ResourceVersion, hk0.Meta.UpdatedAt)
	}
}

// Every write other than last_used_at still notifies and bumps the version, and an operator's write keeps the recorded time.
func TestIntegration_OtherWritesNotifyBumpAndKeepLastUsed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := storagetest.Pool(t)
	keyIDs, hostKeyIDs := seedCredentials(t, pool, 1)
	s := newStores(pool)
	at := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	if err := s.touch.TouchKeys(ctx, entries(keyIDs, at)); err != nil {
		t.Fatal(err)
	}
	if err := s.touch.TouchHostKeys(ctx, entries(hostKeyIDs, at)); err != nil {
		t.Fatal(err)
	}
	conn := listen(t, ctx, pool)

	writes := []struct {
		name  string
		id    string
		write func() error
	}{
		{"key metadata", keyIDs[0], func() error {
			k := getKey(t, s, keyIDs[0])
			k.Meta.DisplayName = "renamed"
			return s.keys.Upsert(ctx, k)
		}},
		{"key spec", keyIDs[0], func() error {
			k := getKey(t, s, keyIDs[0])
			off := false
			k.Spec.Enabled = &off
			k.Status = key.ObservedStatus{} // an operator body without status
			return s.keys.Upsert(ctx, k)
		}},
		{"host key spec", hostKeyIDs[0], func() error {
			hk := getHostKey(t, s, hostKeyIDs[0])
			hk.Spec.PricingStrategy = hostkey.StrategySub
			hk.Status = hostkey.Status{}
			return s.hostKeys.Upsert(ctx, hk)
		}},
		{"host key oauth status", hostKeyIDs[0], func() error {
			return s.hostKeys.SetCredentialStatus(ctx, hostKeyIDs[0], hostkey.CredentialStatus{State: hostkey.CredentialRevoked, At: at})
		}},
		{"host key raw status", hostKeyIDs[0], func() error {
			_, err := pool.Exec(ctx, `UPDATE secrets SET status = '{"credential":{"state":"ok","at":"2026-11-15T10:31:00Z"}}' WHERE id = $1`, hostKeyIDs[0])
			return err
		}},
	}
	for _, w := range writes {
		version := func() string {
			if w.id == keyIDs[0] {
				return getKey(t, s, w.id).Meta.ResourceVersion
			}
			return getHostKey(t, s, w.id).Meta.ResourceVersion
		}
		before := version()
		if err := w.write(); err != nil {
			t.Fatalf("%s: %v", w.name, err)
		}
		if !notified(ctx, conn, w.id, 500*time.Millisecond) {
			t.Errorf("%s: no notification for %s", w.name, w.id)
		}
		if after := version(); after == before {
			t.Errorf("%s: resourceVersion stayed %s", w.name, after)
		}
	}
	if got := getKey(t, s, keyIDs[0]).Status.LastUsedAt; !sameTime(got, at) {
		t.Errorf("key lastUsedAt after operator writes = %v, want %v", got, at)
	}
	if got := getHostKey(t, s, hostKeyIDs[0]).Status.LastUsedAt; !sameTime(got, at) {
		t.Errorf("host key lastUsedAt after operator writes = %v, want %v", got, at)
	}

	// Deleting still notifies.
	if err := s.keys.Delete(ctx, keyIDs[0]); err != nil {
		t.Fatal(err)
	}
	if !notified(ctx, conn, keyIDs[0], 500*time.Millisecond) {
		t.Error("no notification after deleting the key")
	}
}
