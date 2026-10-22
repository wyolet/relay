package ratelimit

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
)

// The longest gap between two commits of one reservation, on the client defaults: a failed-over key's release waits out its reply (one command) and records the key failure (a read and a write), then the error path commits the inbound and the upstream reservation (a script each, up to five commands). A commit script resent within one call spans fewer commands.
func TestCommitGuardTTLCoversCommitGap(t *testing.T) {
	command := kv.DefaultPoolTimeout + kv.DefaultWriteTimeout + kv.DefaultReadTimeout + kv.DefaultDialTimeout
	gap := command + 2*command + 5*command + 5*command
	if commitGuardTTL < gap {
		t.Fatalf("commitGuardTTL = %s, shorter than the %s commit gap", commitGuardTTL, gap)
	}
}

func TestCommitGuardMem(t *testing.T) {
	runCommitGuardSuite(t, func(t *testing.T) kvtest.ScriptStore {
		m := newTestStore(t)
		RegisterScripts(m)
		return m
	})
}

// runCommitGuardSuite pins the idempotency guard: a commit writes it with commitGuardTTL, a repeated commit is a no-op while it lives, and once it expires the same commit settles again.
func runCommitGuardSuite(t *testing.T, newStore func(t *testing.T) kvtest.ScriptStore) {
	ctx := context.Background()
	obs := Observations{Tokens: map[string]int64{"input": 6, "output": 4}}

	t.Run("written_with_ttl", func(t *testing.T) {
		rec := kvtest.NewRecording(newStore(t))
		l := New(rec, discardLog(), nil)
		rules := []Rule{tokRule("guard:tok", "tokens", 15, time.Hour)}
		res, err := l.Reserve(ctx, "guard-scope", rules)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Commit(ctx, res, obs); err != nil {
			t.Fatal(err)
		}
		calls := rec.Named("limit.commit")
		if len(calls) != 1 {
			t.Fatalf("commit scripts = %d, want 1", len(calls))
		}
		if got, want := calls[0].Keys[0], commitGuardKey("guard-scope", res.ID); got != want {
			t.Fatalf("guard key = %q, want %q", got, want)
		}
		if got := calls[0].Args[1]; got != commitGuardTTL.Milliseconds() {
			t.Fatalf("guard ttl arg = %v, want %d", got, commitGuardTTL.Milliseconds())
		}
		if _, err := rec.Get(ctx, commitGuardKey("guard-scope", res.ID)); err != nil {
			t.Fatalf("guard key: %v", err)
		}

		// 10 tokens against 15: a second count would exceed it.
		if err := l.Commit(ctx, res, obs); err != nil {
			t.Fatal(err)
		}
		if _, err := l.Reserve(ctx, "guard-scope", rules); err != nil {
			t.Fatalf("repeated commit counted again: %v", err)
		}
	})

	t.Run("expiry_ends_idempotency", func(t *testing.T) {
		store := newStore(t)
		l := New(store, discardLog(), nil)
		res, err := l.Reserve(ctx, "guard-expiry", []Rule{tokRule("guard:exp", "tokens", 100, time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		call, err := l.buildCommitCall(res, obs, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		call.Args = slices.Clone(call.Args)
		call.Args[1] = int64(200)
		run := func() string {
			t.Helper()
			raw, err := store.RunScript(ctx, call.Name, call.Script, call.Keys, call.Args...)
			if err != nil {
				t.Fatal(err)
			}
			return string(raw)
		}
		if got := run(); got == "noop" {
			t.Fatal("first commit was a no-op")
		}
		if got := run(); got != "noop" {
			t.Fatalf("commit inside the guard ttl = %q, want noop", got)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := store.Get(ctx, call.Keys[0]); errors.Is(err, kv.ErrNotFound) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("guard key outlived its 200 ms ttl")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if got := run(); got == "noop" {
			t.Fatal("commit after the guard expired was a no-op")
		}
	})
}
