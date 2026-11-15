//go:build integration

// store_integration_test.go covers the last-used writes against a real
// Postgres: the later-time-wins upsert, deleted rows, and that the writes
// notify no pod. Run with: make test-integration.

package lastused_test

import (
	"context"
	"fmt"
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

// seedCredentials writes n relay keys and n host keys, returning their ids.
func seedCredentials(t *testing.T, pool *pgxpool.Pool, n int) (keyIDs, hostKeyIDs []string) {
	t.Helper()
	ctx := context.Background()
	u := &user.User{ID: meta.NewID(), Username: "lastused-" + meta.NewID()[:8]}
	if err := user.NewStore(pool).Upsert(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	keys := key.NewStore(gen.New(pool))
	reg := secret.NewRegistry()
	reg.Register(secret.KindEnv, secret.EnvResolver{})
	hostKeys := hostkey.NewStore(pool, reg, nil)
	for i := range n {
		id := meta.NewID()
		k := &key.Key{Meta: meta.Metadata{ID: id, Name: "key-" + id[len(id)-12:], Owner: meta.Owner{Kind: meta.OwnerUser, ID: u.ID}}}
		k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: u.ID}
		k.Spec.KeyHash = fmt.Sprintf("%064x", i+1)
		if err := keys.Upsert(ctx, k); err != nil {
			t.Fatalf("seed key: %v", err)
		}
		keyIDs = append(keyIDs, id)

		hid := meta.NewID()
		hk := &hostkey.HostKey{
			Meta: meta.Metadata{ID: hid, Name: "hk-" + hid[len(hid)-12:], Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: hostkey.Spec{HostID: meta.NewID(), AccountID: meta.NewID(), ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindEnv, Env: "LASTUSED_TEST_KEY"}},
		}
		if err := hostKeys.Upsert(ctx, hk); err != nil {
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

func TestIntegration_TouchKeepsTheLaterTime(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	keyIDs, hostKeyIDs := seedCredentials(t, pool, 2)
	s := lastused.NewStore(gen.New(pool))
	early := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	late := early.Add(5 * time.Minute)

	if err := s.TouchKeys(ctx, entries(keyIDs[:1], late)); err != nil {
		t.Fatal(err)
	}
	// A pod with an older time writes after: the stored time stays.
	if err := s.TouchKeys(ctx, entries(keyIDs, early)); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchHostKeys(ctx, entries(hostKeyIDs[1:], early)); err != nil {
		t.Fatal(err)
	}
	got, err := s.KeysLastUsed(ctx, append(keyIDs, "never-written"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[keyIDs[0]].Equal(late) || !got[keyIDs[1]].Equal(early) {
		t.Errorf("keys = %v, want %s at %v and %s at %v", got, keyIDs[0], late, keyIDs[1], early)
	}
	hk, err := s.HostKeysLastUsed(ctx, hostKeyIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(hk) != 1 || !hk[hostKeyIDs[1]].Equal(early) {
		t.Errorf("host keys = %v, want only %s at %v", hk, hostKeyIDs[1], early)
	}
}

// Ids of keys deleted after they served are skipped, and a delete takes the time with it.
func TestIntegration_TouchSkipsDeletedKeys(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	keyIDs, hostKeyIDs := seedCredentials(t, pool, 1)
	s := lastused.NewStore(gen.New(pool))
	at := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	gone := meta.NewID()
	if err := s.TouchKeys(ctx, entries([]string{keyIDs[0], gone}, at)); err != nil {
		t.Fatalf("TouchKeys with a deleted id: %v", err)
	}
	if err := s.TouchHostKeys(ctx, entries([]string{gone, hostKeyIDs[0]}, at)); err != nil {
		t.Fatalf("TouchHostKeys with a deleted id: %v", err)
	}
	if err := key.NewStore(gen.New(pool)).Delete(ctx, keyIDs[0]); err != nil {
		t.Fatal(err)
	}
	got, err := s.KeysLastUsed(ctx, []string{keyIDs[0], gone})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("keys after delete = %v, want none", got)
	}
}

// Two pods flushing the same rows at once, each its own times: every row ends at the later one.
func TestIntegration_ConcurrentFlushersKeepTheLaterTime(t *testing.T) {
	ctx := context.Background()
	pool := storagetest.Pool(t)
	keyIDs, hostKeyIDs := seedCredentials(t, pool, 20)
	s := lastused.NewStore(gen.New(pool))
	early := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	late := early.Add(time.Minute)
	for range 10 {
		var wg sync.WaitGroup
		errs := make(chan error, 4)
		for _, at := range []time.Time{early, late} {
			wg.Go(func() { errs <- s.TouchKeys(ctx, entries(keyIDs, at)) })
			wg.Go(func() { errs <- s.TouchHostKeys(ctx, entries(hostKeyIDs, at)) })
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	got, err := s.KeysLastUsed(ctx, keyIDs)
	if err != nil {
		t.Fatal(err)
	}
	hk, err := s.HostKeysLastUsed(ctx, hostKeyIDs)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []map[string]time.Time{got, hk} {
		if len(m) != 20 {
			t.Fatalf("rows = %d, want 20", len(m))
		}
		for id, at := range m {
			if !at.Equal(late) {
				t.Errorf("%s = %v, want %v", id, at, late)
			}
		}
	}
}

// The writes land once a minute from every pod; they must not notify pods to rebuild the catalog.
func TestIntegration_TouchFiresNoCatalogNotify(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool := storagetest.Pool(t)
	keyIDs, hostKeyIDs := seedCredentials(t, pool, 1)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN catalog_events"); err != nil {
		t.Fatal(err)
	}
	s := lastused.NewStore(gen.New(pool))
	at := time.Date(2026, 11, 15, 10, 30, 0, 0, time.UTC)
	for _, a := range []time.Time{at, at.Add(time.Minute)} {
		if err := s.TouchKeys(ctx, entries(keyIDs, a)); err != nil {
			t.Fatal(err)
		}
		if err := s.TouchHostKeys(ctx, entries(hostKeyIDs, a)); err != nil {
			t.Fatal(err)
		}
	}
	wait, stop := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stop()
	if n, err := conn.Conn().WaitForNotification(wait); err == nil {
		t.Errorf("notification %q on %s after last-used writes, want none", n.Payload, n.Channel)
	}
	// The listener does hear a write to the key itself.
	if err := key.NewStore(gen.New(pool)).Delete(ctx, keyIDs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
		t.Errorf("no notification after deleting the key: %v", err)
	}
}
