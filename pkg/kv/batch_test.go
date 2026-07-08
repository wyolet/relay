package kv

import (
	"context"
	"testing"
)

// TestMemRunScriptBatch: the Mem batch runner executes each call in order,
// returns positional results, and applies every call's side effects — the
// in-process equivalent of a Redis pipeline of independent scripts.
func TestMemRunScriptBatch(t *testing.T) {
	m := NewMem()
	t.Cleanup(func() { _ = m.Close() })

	// A trivial script: SET KEYS[1] = ARGV[1], return the prior value or "".
	m.RegisterScript("test.setget", func(ctx context.Context, s *Mem, keys []string, args []any) ([]byte, error) {
		var old []byte
		if b, err := s.Get(ctx, keys[0]); err == nil {
			old = b
		}
		_ = s.Set(ctx, keys[0], []byte(args[0].(string)), 0)
		return old, nil
	})

	_ = m.Set(context.Background(), "b", []byte("old-b"), 0)

	var bs BatchScripter = m
	results := bs.RunScriptBatch(context.Background(), []ScriptCall{
		{Name: "test.setget", Keys: []string{"a"}, Args: []any{"new-a"}},
		{Name: "test.setget", Keys: []string{"b"}, Args: []any{"new-b"}},
	})

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Err != nil || string(results[0].Value) != "" {
		t.Errorf("result[0] = %q, err=%v; want empty prior value", results[0].Value, results[0].Err)
	}
	if results[1].Err != nil || string(results[1].Value) != "old-b" {
		t.Errorf("result[1] = %q, err=%v; want prior value old-b", results[1].Value, results[1].Err)
	}

	// Side effects landed.
	for k, want := range map[string]string{"a": "new-a", "b": "new-b"} {
		got, err := m.Get(context.Background(), k)
		if err != nil || string(got) != want {
			t.Errorf("Get(%s) = %q err=%v, want %q", k, got, err, want)
		}
	}

	// Empty batch is a clean no-op.
	if r := bs.RunScriptBatch(context.Background(), nil); len(r) != 0 {
		t.Errorf("empty batch returned %d results, want 0", len(r))
	}
}
