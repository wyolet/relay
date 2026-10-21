//go:build integration

package kvtest

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/kv"
)

// ExhaustedPool connects a kv.Redis on cfg with a one-connection pool whose connection is held by a command a proxy keeps from the server until cleanup, so every other call waits PoolTimeout for a connection and fails.
func ExhaustedPool(t testing.TB, cfg kv.RedisConfig) *kv.Redis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		stalled atomic.Bool
		held    = make(chan struct{}, 1)
		release = make(chan struct{})
		conns   sync.WaitGroup
	)
	t.Cleanup(func() {
		_ = ln.Close()
		conns.Wait()
	})
	upstream := cfg.Addr
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer conns.Done()
				forward(c, upstream, &stalled, held, release, &conns)
			}()
		}
	}()

	cfg.Addr = ln.Addr().String()
	cfg.PoolSize, cfg.MinIdleConns = 1, 0
	cfg.PoolTimeout = 50 * time.Millisecond
	// The held command must keep its connection until cleanup, not give it back at a read timeout.
	cfg.ReadTimeout = time.Minute
	s := Connect(t, cfg)

	stalled.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.Set(context.Background(), "{kvtest}:hold", []byte("1"), time.Minute)
	}()
	t.Cleanup(func() {
		close(release)
		<-done
	})
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("kvtest: the holding command never reached the proxy")
	}
	return s
}

// forward relays client to the server at addr, holding client bytes back while stalled until release closes.
func forward(client net.Conn, addr string, stalled *atomic.Bool, held chan<- struct{}, release <-chan struct{}, conns *sync.WaitGroup) {
	defer client.Close()
	server, err := net.Dial("tcp", addr)
	if err != nil {
		return
	}
	defer server.Close()
	conns.Add(1)
	go func() {
		defer conns.Done()
		_, _ = io.Copy(client, server)
		_ = client.Close()
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := client.Read(buf)
		if n > 0 {
			if stalled.Load() {
				select {
				case held <- struct{}{}:
				default:
				}
				<-release
			}
			if _, err := server.Write(buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}
