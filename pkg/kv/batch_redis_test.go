//go:build integration

package kv_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

func TestBatchRedis(t *testing.T) {
	cfg := kvtest.Config(t)
	runBatchSuite(t, func(t *testing.T) kv.Store { return kvtest.Connect(t, cfg) })
}

// A batch that waits out PoolTimeout without a connection fails every command with ErrUnavailable, as a single command would.
func TestBatchRedis_PoolTimeoutFailsEveryResult(t *testing.T) {
	s := kvtest.ExhaustedPool(t, kvtest.Config(t))
	ctx := context.Background()

	b := kv.NewBatch(s)
	results := []*kv.BatchResult{
		b.Script(echoCall("{t}:k", "a")), b.Get("{t}:g"), b.Incr("{t}:ctr", 1), b.ExpireNX("{t}:ctr", time.Minute),
	}
	b.Exec(ctx)
	for i, r := range results {
		if !errors.Is(r.Err, kv.ErrUnavailable) || !errors.Is(r.Err, redis.ErrPoolTimeout) {
			t.Errorf("result %d: err = %v, want ErrUnavailable from the pool wait", i, r.Err)
		}
	}

	for i, r := range s.RunScriptBatch(ctx, []kv.ScriptCall{echoCall("{t}:k", "a"), echoCall("{u}:k", "b")}) {
		if !errors.Is(r.Err, kv.ErrUnavailable) {
			t.Errorf("RunScriptBatch %d: err = %v, want ErrUnavailable", i, r.Err)
		}
	}
}

// A script evicted between the SHA preload and the pipeline answers NOSCRIPT inside the batch; that one script reruns alone and the batch still succeeds.
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
