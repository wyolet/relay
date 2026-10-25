package kv_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

const echoScript = `return KEYS[1] .. ":" .. ARGV[1]`

func echoCall(key, arg string) kv.ScriptCall {
	return kv.ScriptCall{Name: "test.echo", Script: echoScript, Keys: []string{key}, Args: []any{arg}}
}

func registerEcho(s kv.Store) {
	if m, ok := s.(*kv.Mem); ok {
		m.RegisterScript("test.echo", func(_ context.Context, _ *kv.Mem, keys []string, args []any) ([]byte, error) {
			return []byte(keys[0] + ":" + args[0].(string)), nil
		})
	}
}

// runBatchSuite pins kv.Batch on one backend: results match the single commands, EXPIRE NX never moves an existing deadline, a script returns what RunScript would, and Exec empties the batch. Subtests may share a keyspace, so each writes keys of its own.
func runBatchSuite(t *testing.T, factory storeFactory) {
	t.Run("get_incr", func(t *testing.T) {
		ctx := context.Background()
		s := factory(t)
		if err := s.Set(ctx, "{t}:present", []byte("v"), time.Minute); err != nil {
			t.Fatal(err)
		}
		b := kv.NewBatch(s)
		present := b.Get("{t}:present")
		missing := b.Get("{t}:missing")
		n1 := b.Incr("{t}:ctr", 1)
		n2 := b.Incr("{t}:ctr", 2)
		b.Exec(ctx)
		if present.Err != nil || string(present.Value) != "v" {
			t.Fatalf("present = %q, %v", present.Value, present.Err)
		}
		if !errors.Is(missing.Err, kv.ErrNotFound) || missing.Value != nil {
			t.Fatalf("missing = %q, %v; want ErrNotFound", missing.Value, missing.Err)
		}
		if n1.Err != nil || n1.Int != 1 || n2.Err != nil || n2.Int != 3 {
			t.Fatalf("incr = %d/%v, %d/%v; want 1, 3", n1.Int, n1.Err, n2.Int, n2.Err)
		}
	})

	t.Run("expire_nx", func(t *testing.T) {
		ctx := context.Background()
		s := factory(t)
		if err := s.Set(ctx, "{t}:persistent", []byte("1"), 0); err != nil {
			t.Fatal(err)
		}
		if err := s.Set(ctx, "{t}:short", []byte("1"), 300*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		b := kv.NewBatch(s)
		first := b.ExpireNX("{t}:persistent", time.Hour)
		again := b.ExpireNX("{t}:persistent", time.Hour)
		short := b.ExpireNX("{t}:short", time.Hour)
		missing := b.ExpireNX("{t}:missing", time.Hour)
		b.Exec(ctx)
		for name, r := range map[string]*kv.BatchResult{"first": first, "again": again, "short": short, "missing": missing} {
			if r.Err != nil {
				t.Fatalf("%s: %v", name, r.Err)
			}
		}
		if first.Int != 1 || again.Int != 0 || short.Int != 0 || missing.Int != 0 {
			t.Fatalf("set = first %d, again %d, short %d, missing %d; want 1, 0, 0, 0", first.Int, again.Int, short.Int, missing.Int)
		}
		time.Sleep(600 * time.Millisecond)
		if _, err := s.Get(ctx, "{t}:short"); !errors.Is(err, kv.ErrNotFound) {
			t.Fatalf("short-lived key after its deadline: %v; EXPIRE NX must not extend it", err)
		}
		if _, err := s.Get(ctx, "{t}:persistent"); err != nil {
			t.Fatalf("persistent key: %v", err)
		}
	})

	t.Run("script", func(t *testing.T) {
		ctx := context.Background()
		s := factory(t)
		registerEcho(s)
		b := kv.NewBatch(s)
		r := b.Script(echoCall("{t}:k", "a"))
		g := b.Get("{t}:absent")
		b.Exec(ctx)
		if r.Err != nil || string(r.Value) != "{t}:k:a" {
			t.Fatalf("script = %q, %v", r.Value, r.Err)
		}
		if !errors.Is(g.Err, kv.ErrNotFound) {
			t.Fatalf("get beside a script = %v", g.Err)
		}
		r2 := b.Script(echoCall("{t}:k", "b"))
		b.Exec(ctx)
		if r2.Err != nil || string(r2.Value) != "{t}:k:b" {
			t.Fatalf("script after a first Exec = %q, %v", r2.Value, r2.Err)
		}
	})

	t.Run("command_error_stays_local", func(t *testing.T) {
		ctx := context.Background()
		s := factory(t)
		if err := s.Set(ctx, "{t}:text", []byte("not a number"), time.Minute); err != nil {
			t.Fatal(err)
		}
		b := kv.NewBatch(s)
		bad := b.Incr("{t}:text", 1)
		good := b.Incr("{t}:ctr-beside-error", 1)
		missing := b.Get("{t}:missing")
		b.Exec(ctx)
		if bad.Err == nil || errors.Is(bad.Err, kv.ErrNotFound) {
			t.Fatalf("incr on text = %v, want the store's error", bad.Err)
		}
		if good.Err != nil || good.Int != 1 {
			t.Fatalf("incr beside it = %d, %v", good.Int, good.Err)
		}
		if !errors.Is(missing.Err, kv.ErrNotFound) {
			t.Fatalf("get beside it = %v", missing.Err)
		}
	})

	t.Run("shed_when_pool_busy_runs_when_idle", func(t *testing.T) {
		ctx := context.Background()
		s := factory(t)
		registerEcho(s)
		b := kv.NewBatch(s)
		r := b.Script(echoCall("{t}:shed", "a"))
		n := b.Incr("{t}:shed-ctr", 1)
		b.ShedWhenPoolBusy()
		b.Exec(ctx)
		if r.Err != nil || string(r.Value) != "{t}:shed:a" {
			t.Fatalf("script = %q, %v", r.Value, r.Err)
		}
		if n.Err != nil || n.Int != 1 {
			t.Fatalf("incr = %d, %v", n.Int, n.Err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		kv.NewBatch(factory(t)).Exec(context.Background())
	})
}

func TestBatchMem(t *testing.T) {
	runBatchSuite(t, func(t *testing.T) kv.Store {
		s := kv.NewMem()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
