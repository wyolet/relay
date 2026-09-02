//go:build integration

// key_rotate_concurrent_integration_test.go fires concurrent rotations (and a
// plain update) at one key through the real handler. The conditional
// UPDATE that decides the winner lives in Postgres and *key.Store is a
// concrete type, so there is no fake seam for this at the unit layer.
// Run with: make test-integration.
package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	pgmigrations "github.com/wyolet/relay/migrations/postgres"
)

func setupRotateRaceDB(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("RELAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("RELAY_TEST_PG_DSN not set; run via `make test-integration`")
	}
	src, err := iofs.New(pgmigrations.FS, ".")
	if err != nil {
		t.Fatalf("migrate src: %v", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, dsn)
	if err != nil {
		t.Fatalf("migrate init: %v", err)
	}
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

// N clients rotate the same key at once, with a plain PUT racing them. A rotation
// that read a superseded hash must be refused with 409 rather than handed a
// plaintext that authenticates nothing, and exactly one plaintext may end up
// matching the stored hash. (More than one 200 is legitimate: a request that
// reads after an earlier rotation committed is a second rotation, not a
// lost update — what may never happen is two live hashes.)
func TestIntegration_ConcurrentRotatesLeaveOneLiveHash(t *testing.T) {
	pool, ctx := setupRotateRaceDB(t)
	_, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}

	u := &user.User{ID: meta.NewID(), Username: "rotate-race-" + meta.NewID()[:8]}
	users := user.NewStore(gen.New(pool))
	if err := users.Upsert(ctx, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _ = users.Delete(ctx, u.ID) })

	gen0, err := key.Generate()
	if err != nil {
		t.Fatalf("key.Generate: %v", err)
	}
	k := &key.Key{Meta: meta.Metadata{
		ID: meta.NewID(), Name: "rotate-race", Owner: meta.Owner{Kind: meta.OwnerUser, ID: u.ID},
	}}
	k.Spec.Principal = key.Principal{Kind: key.PrincipalUser, ID: u.ID}
	k.Spec.KeyHash = gen0.KeyHash
	k.Spec.Prefix = gen0.Prefix
	if err := stores.Key.Upsert(ctx, k); err != nil {
		t.Fatalf("upsert key: %v", err)
	}
	t.Cleanup(func() { _ = stores.Key.Delete(ctx, k.Meta.ID) })

	deps := mountDeps(t)
	deps.Stores = stores
	r := chi.NewRouter()
	Mount(r, deps)

	const rotations = 16
	type outcome struct {
		status    int
		plaintext string
	}
	results := make([]outcome, rotations)
	var wg sync.WaitGroup
	for i := 0; i < rotations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost,
				fmt.Sprintf("/keys/by-id/%s/rotate", k.Meta.ID), strings.NewReader(`{"graceSeconds":0}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+deps.AdminToken)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			out := outcome{status: w.Code}
			if w.Code == http.StatusOK {
				var body struct {
					Plaintext string `json:"plaintext"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Errorf("decode rotate body: %v", err)
				}
				out.plaintext = body.Plaintext
			}
			results[i] = out
		}(i)
	}
	// A plain update racing the rotations: it must not be able to resurrect
	// a superseded hash, which is what the updated_at half of the CAS is for.
	wg.Add(1)
	go func() {
		defer wg.Done()
		body := `{"metadata":{"displayName":"renamed mid-rotation"}}`
		req := httptest.NewRequest(http.MethodPut,
			fmt.Sprintf("/keys/by-id/%s", k.Meta.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+deps.AdminToken)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	wg.Wait()

	var granted []string
	conflicts := 0
	for i, res := range results {
		switch res.status {
		case http.StatusOK:
			granted = append(granted, res.plaintext)
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("rotation %d returned %d, want 200 or 409", i, res.status)
		}
	}
	if len(granted) == 0 {
		t.Fatal("no rotation succeeded")
	}
	if conflicts == 0 {
		t.Fatalf("%d simultaneous rotations all succeeded — the CAS refused nobody", rotations)
	}

	stored, err := stores.Key.Get(ctx, k.Meta.ID)
	if err != nil || stored == nil {
		t.Fatalf("re-read key: %v", err)
	}
	live := 0
	for _, plaintext := range granted {
		sum := sha256.Sum256([]byte(plaintext))
		if hex.EncodeToString(sum[:]) == stored.Spec.KeyHash {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("%d of %d handed-out plaintexts match the stored hash, want exactly 1",
			live, len(granted))
	}
	if stored.Spec.PreviousKeyHash != "" || stored.Spec.GraceUntil != nil {
		t.Fatalf("graceSeconds 0 left a previous hash live: prev=%q until=%v",
			stored.Spec.PreviousKeyHash, stored.Spec.GraceUntil)
	}
}
