package kv_test

// redis_timeout_test.go drives kv.Redis against a scripted RESP server that
// can leave a command unanswered, to pin how long a call may wait and that a
// command which may have run is never sent a second time.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// noReply leaves a command hanging; dropConn closes the connection after reading it.
const (
	noReply  = ""
	dropConn = "drop"
)

type fakeRedis struct {
	addr string

	mu   sync.Mutex
	seen []string
	// reply answers a command by name; n is how many times that name was seen, this one included.
	reply func(name string, n int) string
}

func newFakeRedis(t *testing.T, reply func(name string, n int) string) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRedis{addr: ln.Addr().String(), reply: reply}
	var conns sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		conns.Wait()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				f.serve(c)
			}()
		}
	}()
	return f
}

func (f *fakeRedis) serve(c net.Conn) {
	defer c.Close()
	rd := bufio.NewReader(c)
	for {
		args, err := readCommand(rd)
		if err != nil {
			return
		}
		name := strings.ToUpper(args[0])
		var out string
		switch name {
		case "HELLO":
			out = "-ERR unknown command 'HELLO'\r\n"
		case "PING":
			out = "+PONG\r\n"
		case "CLIENT", "SELECT":
			out = "+OK\r\n"
		case "SCRIPT":
			sha := strings.Repeat("a", 40)
			out = "$40\r\n" + sha + "\r\n"
		default:
			f.mu.Lock()
			f.seen = append(f.seen, name)
			n := 0
			for _, s := range f.seen {
				if s == name {
					n++
				}
			}
			f.mu.Unlock()
			out = f.reply(name, n)
		}
		if out == noReply {
			continue
		}
		if out == dropConn {
			return
		}
		if _, err := io.WriteString(c, out); err != nil {
			return
		}
	}
}

func (f *fakeRedis) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.seen {
		if s == name {
			n++
		}
	}
	return n
}

func readCommand(rd *bufio.Reader) ([]string, error) {
	line, err := rd.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("unexpected %q", line)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for range n {
		hdr, err := rd.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(hdr[1:]))
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(rd, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func connectFake(t *testing.T, cfg kv.RedisConfig) *kv.Redis {
	t.Helper()
	s, err := kv.NewRedis(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewRedis: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// within fails unless fn returns within limit, returning its error.
func within(t *testing.T, limit time.Duration, fn func() error) error {
	t.Helper()
	start := time.Now()
	err := fn()
	if d := time.Since(start); d > limit {
		t.Fatalf("call took %v, want under %v", d, limit)
	}
	return err
}

func TestRedisCallGivesUpAtReadTimeout(t *testing.T) {
	f := newFakeRedis(t, func(string, int) string { return noReply })
	s := connectFake(t, kv.RedisConfig{Addr: f.addr, ReadTimeout: 100 * time.Millisecond})

	// A caller with no deadline of its own, as a streamed request or a detached post-flight is.
	err := within(t, 600*time.Millisecond, func() error {
		return s.Set(context.Background(), "k", []byte("v"), time.Minute)
	})
	if !errors.Is(err, kv.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if n := f.count("SET"); n != 1 {
		t.Fatalf("SET sent %d times, want 1", n)
	}
}

func TestRedisScriptNotResentAfterReplyTimeout(t *testing.T) {
	f := newFakeRedis(t, func(string, int) string { return noReply })
	s := connectFake(t, kv.RedisConfig{Addr: f.addr, ReadTimeout: 100 * time.Millisecond})

	err := within(t, 600*time.Millisecond, func() error {
		_, err := s.RunScript(context.Background(), "test.count", `return redis.call('INCR', KEYS[1])`, []string{"{t}:n"})
		return err
	})
	if !errors.Is(err, kv.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	time.Sleep(300 * time.Millisecond) // room for a late resend to arrive
	if n, m := f.count("EVALSHA"), f.count("EVAL"); n != 1 || m != 0 {
		t.Fatalf("EVALSHA sent %d times and EVAL %d, want 1 and 0", n, m)
	}
}

// A connection dropped after the command was sent is not a timeout, so the call budget does not stop a resend; only the retry count does.
func TestRedisScriptNotResentAfterDroppedConnection(t *testing.T) {
	f := newFakeRedis(t, func(string, int) string { return dropConn })
	s := connectFake(t, kv.RedisConfig{Addr: f.addr})

	_, err := s.RunScript(context.Background(), "test.count", `return 1`, []string{"{t}:n"})
	if !errors.Is(err, kv.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if n := f.count("EVALSHA"); n != 1 {
		t.Fatalf("EVALSHA sent %d times, want 1", n)
	}
}

// After a NOSCRIPT the script is reloaded and resent; if that second EVALSHA goes unanswered it may have run, so no EVAL may follow.
func TestRedisScriptNoEvalAfterReloadedCallTimesOut(t *testing.T) {
	f := newFakeRedis(t, func(name string, n int) string {
		if name == "EVALSHA" && n == 1 {
			return "-NOSCRIPT No matching script.\r\n"
		}
		return noReply
	})
	s := connectFake(t, kv.RedisConfig{Addr: f.addr, ReadTimeout: 100 * time.Millisecond})

	_, err := s.RunScript(context.Background(), "test.count", `return 1`, []string{"{t}:n"})
	if !errors.Is(err, kv.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	time.Sleep(300 * time.Millisecond)
	if n, m := f.count("EVALSHA"), f.count("EVAL"); n != 2 || m != 0 {
		t.Fatalf("EVALSHA sent %d times and EVAL %d, want 2 and 0", n, m)
	}
}

func TestRedisPoolWaitBoundedByPoolTimeout(t *testing.T) {
	f := newFakeRedis(t, func(string, int) string { return noReply })
	s := connectFake(t, kv.RedisConfig{
		Addr: f.addr, PoolSize: 1,
		ReadTimeout: time.Second, PoolTimeout: 50 * time.Millisecond,
	})

	held := make(chan struct{})
	go func() {
		defer close(held)
		_ = s.Set(context.Background(), "a", []byte("v"), time.Minute)
	}()
	deadline := time.Now().Add(time.Second)
	for f.count("SET") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first call never reached the server")
		}
		time.Sleep(5 * time.Millisecond)
	}

	err := within(t, 500*time.Millisecond, func() error {
		_, err := s.Get(context.Background(), "b")
		return err
	})
	if !errors.Is(err, kv.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	<-held
}

// A stalled batch costs one read timeout, not one per command; every result
// is unavailable and the script inside it is sent once.
func TestRedisBatchGivesUpOnceAtReadTimeout(t *testing.T) {
	f := newFakeRedis(t, func(string, int) string { return noReply })
	s := connectFake(t, kv.RedisConfig{Addr: f.addr, ReadTimeout: 100 * time.Millisecond})

	b := kv.NewBatch(s)
	results := []*kv.BatchResult{
		b.Script(kv.ScriptCall{Name: "test.count", Script: `return 1`, Keys: []string{"{t}:n"}}),
		b.Get("{a}:k"), b.Get("{b}:k"), b.Incr("{c}:n", 1), b.ExpireNX("{c}:n", time.Minute),
	}
	_ = within(t, 600*time.Millisecond, func() error {
		b.Exec(context.Background())
		return nil
	})
	for i, r := range results {
		if !errors.Is(r.Err, kv.ErrUnavailable) {
			t.Fatalf("result %d: err = %v, want ErrUnavailable", i, r.Err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if n, m := f.count("EVALSHA"), f.count("EVAL"); n != 1 || m != 0 {
		t.Fatalf("EVALSHA sent %d times and EVAL %d, want 1 and 0", n, m)
	}
}

// A missing key in a batch is ErrNotFound, not an outage.
func TestRedisBatchMissIsNotUnavailable(t *testing.T) {
	f := newFakeRedis(t, func(string, int) string { return "$-1\r\n" })
	s := connectFake(t, kv.RedisConfig{Addr: f.addr})

	b := kv.NewBatch(s)
	r := b.Get("{a}:k")
	b.Exec(context.Background())
	if !errors.Is(r.Err, kv.ErrNotFound) || errors.Is(r.Err, kv.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrNotFound alone", r.Err)
	}
}

func TestRedisReplyErrorsClassified(t *testing.T) {
	f := newFakeRedis(t, func(name string, _ int) string {
		switch name {
		case "GET":
			return "-LOADING Redis is loading the dataset in memory\r\n"
		default:
			return "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n"
		}
	})
	s := connectFake(t, kv.RedisConfig{Addr: f.addr})

	if _, err := s.Get(context.Background(), "k"); !errors.Is(err, kv.ErrUnavailable) {
		t.Fatalf("LOADING: err = %v, want ErrUnavailable", err)
	}
	if _, err := s.Incr(context.Background(), "k", 1); err == nil || errors.Is(err, kv.ErrUnavailable) {
		t.Fatalf("WRONGTYPE: err = %v, want a reply error that is not ErrUnavailable", err)
	}
}
