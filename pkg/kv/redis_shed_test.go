package kv_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// waitFor fails the test unless cond holds within a second.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// With the one connection held and the one allowed waiter queued, an admission batch is refused at once with nothing sent, where an unbounded call would wait out the pool timeout.
func TestRedisBatchOrShedRefusedAtWaitBound(t *testing.T) {
	release := make(chan struct{})
	f := newFakeRedis(t, func(name string, _ int) string {
		<-release
		if name == "GET" {
			return "$-1\r\n"
		}
		return "+OK\r\n"
	})
	s := connectFake(t, kv.RedisConfig{
		Addr: f.addr, PoolSize: 1, MaxPoolWaiters: 1,
		ReadTimeout: 5 * time.Second, PoolTimeout: 5 * time.Second,
	})
	ctx := context.Background()

	var calls sync.WaitGroup
	t.Cleanup(func() {
		close(release)
		calls.Wait()
	})
	calls.Go(func() { _ = s.Set(ctx, "{t}:held", []byte("1"), time.Minute) })
	waitFor(t, "the holding SET to reach the server", func() bool { return f.count("SET") == 1 })
	if s.PoolBusy() {
		t.Fatal("PoolBusy with no caller waiting")
	}
	calls.Go(func() { _, _ = s.Get(ctx, "{t}:queued") })
	waitFor(t, "the waiter to queue", s.PoolBusy)

	b := kv.NewBatch(s)
	script := b.Script(kv.ScriptCall{Name: "test.count", Script: `return 1`, Keys: []string{"{t}:n"}})
	get := b.Get("{t}:k")
	b.ShedWhenPoolBusy()
	_ = within(t, 100*time.Millisecond, func() error {
		b.Exec(ctx)
		return nil
	})
	for i, r := range []*kv.BatchResult{script, get} {
		if !errors.Is(r.Err, kv.ErrPoolBusy) || !errors.Is(r.Err, kv.ErrUnavailable) {
			t.Errorf("result %d: err = %v, want ErrPoolBusy, an ErrUnavailable", i, r.Err)
		}
	}
	err := within(t, 100*time.Millisecond, func() error {
		_, err := s.RunScriptOrShed(ctx, "test.count", `return 1`, []string{"{t}:n"})
		return err
	})
	if !errors.Is(err, kv.ErrPoolBusy) {
		t.Errorf("RunScriptOrShed err = %v, want ErrPoolBusy", err)
	}
	if n, m, g := f.count("EVALSHA"), f.count("EVAL"), f.count("GET"); n != 0 || m != 0 || g != 0 {
		t.Errorf("sent EVALSHA %d, EVAL %d, GET %d; want nothing sent", n, m, g)
	}
	if !s.PoolBusy() {
		t.Error("a refused batch released a place it never took")
	}
}

// Once the held and queued calls finish, the count drops back and a bounded batch runs.
func TestRedisBatchOrShedRunsOnceThePoolFrees(t *testing.T) {
	release := make(chan struct{})
	f := newFakeRedis(t, func(name string, _ int) string {
		<-release
		if name == "GET" {
			return "$1\r\nv\r\n"
		}
		return "+OK\r\n"
	})
	s := connectFake(t, kv.RedisConfig{Addr: f.addr, PoolSize: 1, MaxPoolWaiters: 1})
	ctx := context.Background()

	var calls sync.WaitGroup
	calls.Go(func() { _ = s.Set(ctx, "{t}:held", []byte("1"), time.Minute) })
	waitFor(t, "the holding SET to reach the server", func() bool { return f.count("SET") == 1 })
	calls.Go(func() { _, _ = s.Get(ctx, "{t}:queued") })
	waitFor(t, "the waiter to queue", s.PoolBusy)
	close(release)
	calls.Wait()
	if s.PoolBusy() {
		t.Fatal("PoolBusy after every call returned")
	}

	b := kv.NewBatch(s)
	get := b.Get("{t}:k")
	b.ShedWhenPoolBusy()
	b.Exec(ctx)
	if get.Err != nil || string(get.Value) != "v" {
		t.Fatalf("get = %q, %v; want v", get.Value, get.Err)
	}
}

// Without MaxPoolWaiters nothing is shed: the batch queues as ExecBatch does.
func TestRedisBatchOrShedUnboundedByDefault(t *testing.T) {
	f := newFakeRedis(t, func(string, int) string { return "$1\r\nv\r\n" })
	s := connectFake(t, kv.RedisConfig{Addr: f.addr, PoolSize: 1})
	if s.PoolBusy() {
		t.Fatal("PoolBusy with no bound set")
	}
	b := kv.NewBatch(s)
	get := b.Get("{t}:k")
	b.ShedWhenPoolBusy()
	b.Exec(context.Background())
	if get.Err != nil || string(get.Value) != "v" {
		t.Fatalf("get = %q, %v; want v", get.Value, get.Err)
	}
}
