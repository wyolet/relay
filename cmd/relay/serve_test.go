package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"testing"
	"time"

	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/internal/config"
	"github.com/wyolet/relay/pkg/httpheader"
)

// Both listeners bind the configured host through the same path boot uses, and answer there.
func TestListenersBindTheConfiguredHost(t *testing.T) {
	t.Setenv("RELAY_LISTEN_HOST", "127.0.0.1")
	t.Setenv("RELAY_CONTROL_LISTEN_HOST", "")
	t.Setenv("RELAY_PORT", "0")
	t.Setenv("RELAY_CONTROL_PORT", "0")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for name, addr := range map[string]string{"inference": cfg.InferenceAddr(), "control": cfg.ControlAddr()} {
		t.Run(name, func(t *testing.T) {
			srv := &http.Server{Addr: addr, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})}
			bound, errs := serve(srv, nil)
			if bound == nil {
				t.Fatalf("serve %s: %v", addr, <-errs)
			}
			tcp, ok := bound.(*net.TCPAddr)
			if !ok || !tcp.IP.Equal(net.IPv4(127, 0, 0, 1)) {
				t.Fatalf("bound %v, want 127.0.0.1", bound)
			}
			resp, err := http.Get("http://" + bound.String() + "/")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if err := srv.Shutdown(context.Background()); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			if err := <-errs; !errors.Is(err, http.ErrServerClosed) {
				t.Fatalf("serve returned %v, want ErrServerClosed", err)
			}
		})
	}
}

// A taken address fails on the channel boot waits on, as ListenAndServe did.
func TestServeReportsABindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	bound, errs := serve(&http.Server{Addr: ln.Addr().String()}, nil)
	if bound != nil {
		t.Fatalf("serve bound %v on a taken address", bound)
	}
	if err := <-errs; err == nil {
		t.Fatal("no bind error reported")
	}
}

// serveCapped serves a 204 handler with the inference admission built from cfg.
func serveCapped(t *testing.T, cfg *config.Config) string {
	t.Helper()
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	bound, errs := serve(srv, inferenceAdmission(cfg))
	if bound == nil {
		t.Fatalf("serve: %v", <-errs)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return bound.String()
}

func getOn(conn net.Conn, wait time.Duration) (*http.Response, []byte, error) {
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		return nil, nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, nil, err
	}
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

func dialT(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Past the cap and the queue a connection gets relay's overload answer.
func TestServeRefusesPastTheCap(t *testing.T) {
	addr := serveCapped(t, &config.Config{MaxConns: 1, ConnQueue: 0, ConnQueueTimeout: time.Second})
	if _, _, err := getOn(dialT(t, addr), 5*time.Second); err != nil {
		t.Fatalf("first connection: %v", err)
	}
	resp, body, err := getOn(dialT(t, addr), 5*time.Second)
	if err != nil {
		t.Fatalf("second connection: %v", err)
	}
	var apiErr httpapi.APIError
	if err := json.Unmarshal(body, &apiErr); err != nil || apiErr.Err.Code != "overloaded" || apiErr.Err.Message != httpapi.OverloadMessage {
		t.Fatalf("refusal body %q: %v", body, err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != httpapi.RetryAfterShed ||
		resp.Header.Get(httpheader.HeaderShouldRetry) != "true" || resp.Header.Get(inference.HeaderOrigin) != "relay" || !resp.Close {
		t.Fatalf("refusal = %d %v", resp.StatusCode, resp.Header)
	}
}

// Past the cap a new connection waits in the queue for an open one to close instead of being served.
func TestServeQueuesPastTheCap(t *testing.T) {
	addr := serveCapped(t, &config.Config{MaxConns: 1, ConnQueue: 1, ConnQueueTimeout: 5 * time.Second})
	first := dialT(t, addr)
	if _, _, err := getOn(first, 5*time.Second); err != nil {
		t.Fatalf("first connection: %v", err)
	}
	second := dialT(t, addr)
	if _, _, err := getOn(second, 300*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("second connection while the first held the only slot: %v, want it to wait", err)
	}
	_ = first.Close()
	_ = second.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(second), nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("queued connection after the first closed: %v", err)
	}
}

func TestInferenceMaxConns(t *testing.T) {
	if got := inferenceMaxConns(500); got != 500 {
		t.Fatalf("configured 500: got %d", got)
	}
	prev := debug.SetMemoryLimit(900 << 20)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	if got := inferenceMaxConns(0); got != 5760 {
		t.Fatalf("900 MiB memory limit: got %d, want 5760", got)
	}
	debug.SetMemoryLimit(64 << 20)
	if got := inferenceMaxConns(0); got != minDerivedMaxConns {
		t.Fatalf("64 MiB memory limit: got %d, want the floor %d", got, minDerivedMaxConns)
	}
	debug.SetMemoryLimit(math.MaxInt64)
	if got := inferenceMaxConns(0); got != noMemLimitMaxConns {
		t.Fatalf("no memory limit: got %d, want %d", got, noMemLimitMaxConns)
	}
}

// Above the floor, the derived cap's connections at their budgeted cost stay within 40 % of the memory limit and use most of it.
func TestInferenceMaxConnsStaysWithinMemoryShare(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	for _, limit := range []int64{256 << 20, 512 << 20, 900 << 20, 1000 << 20, 3 << 30, 7<<30 + 123} {
		debug.SetMemoryLimit(limit)
		conns := int64(inferenceMaxConns(0))
		share := limit * 2 / 5
		if used := conns * connBudgetBytes; used > share || used <= share-connBudgetBytes {
			t.Errorf("limit %d MiB: %d connections budget %d MiB, want at most and within one connection of %d MiB", limit>>20, conns, used>>20, share>>20)
		}
	}
}
