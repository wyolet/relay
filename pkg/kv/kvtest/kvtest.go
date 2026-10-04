//go:build integration

// Package kvtest gives every integration test a Redis/Valkey keyspace of its
// own on the server named by RELAY_TEST_REDIS_ADDR, so test packages run in
// parallel against one server despite fixed key names and prefix scans.
//
// Isolation is by logical database: a test leases one of the indexes 1..15
// through a key on database 0, and the database is flushed and the lease
// released on cleanup. A killed run's leases expire on their own. Cluster
// and Sentinel topologies are out of scope. Tests skip when
// RELAY_TEST_REDIS_ADDR is unset.
package kvtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wyolet/relay/pkg/kv"
)

// EnvAddr names the server the test keyspaces live on.
const EnvAddr = "RELAY_TEST_REDIS_ADDR"

const (
	firstDB = 1
	// Sixteen databases is the server default, and a CI service container
	// cannot pass server arguments to raise it.
	lastDB       = 15
	leaseTTL     = 10 * time.Minute
	leaseWait    = 2 * time.Minute
	leasePolling = 100 * time.Millisecond
)

// releaseLease deletes the lease only while it still holds this test's token,
// so an expired lease taken over by another test is left alone.
var releaseLease = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
	return redis.call("DEL", KEYS[1])
end
return 0`)

// Config returns a kv.RedisConfig for one test on the server named by
// RELAY_TEST_REDIS_ADDR, holding a database index of its own for the test's
// lifetime. Skips when the variable is unset.
func Config(t testing.TB) kv.RedisConfig {
	t.Helper()
	addr := os.Getenv(EnvAddr)
	if addr == "" {
		t.Skip(EnvAddr + " not set; run via `make test-integration`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), leaseWait)
	defer cancel()

	control := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = control.Close() })

	token := randomToken(t)
	db, key := acquire(ctx, t, control, token)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		data := redis.NewClient(&redis.Options{Addr: addr, DB: db})
		defer data.Close()
		if err := data.FlushDB(ctx).Err(); err != nil {
			t.Errorf("kvtest: flush db %d: %v", db, err)
		}
		if err := releaseLease.Run(ctx, control, []string{key}, token).Err(); err != nil {
			t.Errorf("kvtest: release db %d: %v", db, err)
		}
	})
	return kv.RedisConfig{Addr: addr, DB: db}
}

// acquire polls the indexes until one lease is free, then flushes the
// database in case a killed run left keys behind.
func acquire(ctx context.Context, t testing.TB, control *redis.Client, token string) (int, string) {
	t.Helper()
	for {
		for db := firstDB; db <= lastDB; db++ {
			key := "kvtest:lease:" + strconv.Itoa(db)
			ok, err := control.SetNX(ctx, key, token, leaseTTL).Result()
			if err != nil {
				t.Fatalf("kvtest: lease db %d: %v", db, err)
			}
			if !ok {
				continue
			}
			data := redis.NewClient(&redis.Options{Addr: control.Options().Addr, DB: db})
			err = data.FlushDB(ctx).Err()
			_ = data.Close()
			if err != nil {
				t.Fatalf("kvtest: flush db %d: %v", db, err)
			}
			return db, key
		}
		select {
		case <-ctx.Done():
			t.Fatalf("kvtest: no free database on %s within %s", control.Options().Addr, leaseWait)
		case <-time.After(leasePolling):
		}
	}
}

func randomToken(t testing.TB) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("kvtest: token: %v", err)
	}
	return hex.EncodeToString(b)
}
