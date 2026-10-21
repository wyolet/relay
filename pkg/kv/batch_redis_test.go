//go:build integration

package kv_test

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestBatchRedis(t *testing.T) {
	cfg := kvtest.Config(t)
	runBatchSuite(t, func(t *testing.T) kv.Store { return kvtest.Connect(t, cfg) })
}

// A script evicted between the SHA preload and the pipeline answers NOSCRIPT
// inside the batch; that one script reruns alone and the batch still succeeds.
func TestBatchRedis_NoScriptRerunsAlone(t *testing.T) {
	cfg := kvtest.Config(t)
	s := kvtest.Connect(t, cfg)
	ctx := context.Background()

	if _, err := s.RunScript(ctx, "test.echo", echoScript, []string{"{t}:k"}, "warm"); err != nil {
		t.Fatal(err)
	}
	raw := redis.NewClient(&redis.Options{Addr: cfg.Addr, DB: cfg.DB})
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("SCRIPT FLUSH: %v", err)
	}

	b := kv.NewBatch(s)
	r := b.Script(echoCall("{t}:k", "a"))
	n := b.Incr("{t}:ctr", 1)
	b.Exec(ctx)
	if r.Err != nil || string(r.Value) != "{t}:k:a" {
		t.Fatalf("script after flush = %q, %v", r.Value, r.Err)
	}
	if n.Err != nil || n.Int != 1 {
		t.Fatalf("incr beside it = %d, %v", n.Int, n.Err)
	}
}
