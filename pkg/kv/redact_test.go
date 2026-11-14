package kv

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

const secretTail = "s3cr3t-session-token"

func TestRedactKey(t *testing.T) {
	// A leading {tag} survives; anything else is the first four bytes of the key's sha256.
	for key, want := range map[string]string{
		"{secret_health}:abc" + secretTail: "{secret_health}",
		"{rl}:" + secretTail:               "{rl}",
		"{rl}":                             "{rl}",
		"sess:" + secretTail:               "sha256:9728832c",
		"{unterminated:" + secretTail:      "sha256:a6048998",
		"}{x}":                             "sha256:fe012f7d",
		"":                                 "sha256:e3b0c442",
	} {
		if got := redactKey(key); got != want {
			t.Errorf("redactKey(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestSlowRedisOpLogOmitsKey(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// PoolStats needs no live server, so an unconnected client is enough.
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = client.Close() })
	r := &Redis{client: client}

	// Each call opens a fresh log interval, so both are logged.
	slowLastLogNanos.Store(0)
	r.trackSlow("get", "sess:"+secretTail, time.Now().Add(-2*slowOpThreshold), nil)
	slowLastLogNanos.Store(0)
	r.trackSlow("set", "{sess}:"+secretTail, time.Now().Add(-2*slowOpThreshold), nil)

	out := buf.String()
	if strings.Count(out, "slow or failed redis ops") != 2 {
		t.Fatalf("slow path not taken, log: %q", out)
	}
	if strings.Contains(out, secretTail) {
		t.Fatalf("slow-op log leaks the key: %q", out)
	}
	if !strings.Contains(out, "key={sess}") {
		t.Fatalf("slow-op log lost the hash tag: %q", out)
	}
}

func TestMemIncrErrorOmitsKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	key := "sess:" + secretTail
	if err := s.Set(ctx, key, []byte("not-a-number"), 0); err != nil {
		t.Fatal(err)
	}
	_, err := s.Incr(ctx, key, 1)
	const want = `state: Incr on non-integer key sha256:9728832c: strconv.ParseInt: parsing "not-a-number": invalid syntax`
	if err == nil || err.Error() != want {
		t.Fatalf("Incr on text: err = %v, want %q", err, want)
	}
	if v, _ := s.Get(ctx, key); string(v) != "not-a-number" {
		t.Fatalf("a failed Incr changed the value to %q", v)
	}
}
