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
	cases := map[string]string{
		"{secret_health}:abc" + secretTail: "{secret_health}",
		"{rl}:" + secretTail:               "{rl}",
	}
	for key, want := range cases {
		if got := redactKey(key); got != want {
			t.Errorf("redactKey(%q) = %q, want %q", key, got, want)
		}
	}
	for _, key := range []string{"sess:" + secretTail, "{unterminated:" + secretTail, ""} {
		got := redactKey(key)
		if !strings.HasPrefix(got, "sha256:") || strings.Contains(got, secretTail) {
			t.Errorf("redactKey(%q) = %q, want a sha256 digest", key, got)
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

	r.trackSlow("get", "sess:"+secretTail, time.Now().Add(-2*slowOpThreshold), nil)
	r.trackSlow("set", "{sess}:"+secretTail, time.Now().Add(-2*slowOpThreshold), nil)

	out := buf.String()
	if strings.Count(out, "slow redis op") != 2 {
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
	if err == nil {
		t.Fatal("want error on non-integer Incr")
	}
	if strings.Contains(err.Error(), secretTail) {
		t.Fatalf("Incr error leaks the key: %v", err)
	}
}
