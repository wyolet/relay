package main

import (
	"bufio"
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"runtime/debug"
	"testing"
	"time"

	"github.com/wyolet/relay/internal/config"
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
			bound, errs := serve(srv, 0)
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
	bound, errs := serve(&http.Server{Addr: ln.Addr().String()}, 0)
	if bound != nil {
		t.Fatalf("serve bound %v on a taken address", bound)
	}
	if err := <-errs; err == nil {
		t.Fatal("no bind error reported")
	}
}

// Past the cap a new connection waits for an open one to close instead of being served.
func TestServeCapsOpenConnections(t *testing.T) {
	srv := &http.Server{Addr: "127.0.0.1:0", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})}
	bound, errs := serve(srv, 1)
	if bound == nil {
		t.Fatalf("serve: %v", <-errs)
	}
	t.Cleanup(func() { _ = srv.Close() })

	get := func(conn net.Conn, wait time.Duration) error {
		if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		_, err := http.ReadResponse(bufio.NewReader(conn), nil)
		return err
	}
	first, err := net.Dial("tcp", bound.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := get(first, 5*time.Second); err != nil {
		t.Fatalf("first connection: %v", err)
	}
	second, err := net.Dial("tcp", bound.String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := get(second, 300*time.Millisecond); err == nil {
		t.Fatal("a second connection was served while the first held the only slot")
	}

	first.Close()
	third, err := net.Dial("tcp", bound.String())
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	_ = second.Close()
	if err := get(third, 5*time.Second); err != nil {
		t.Fatalf("a connection after the first closed: %v", err)
	}
}

func TestInferenceMaxConns(t *testing.T) {
	if got := inferenceMaxConns(500); got != 500 {
		t.Fatalf("configured 500: got %d", got)
	}
	prev := debug.SetMemoryLimit(900 << 20)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })
	if got := inferenceMaxConns(0); got != 14400 {
		t.Fatalf("900 MiB memory limit: got %d, want 14400", got)
	}
	debug.SetMemoryLimit(16 << 20)
	if got := inferenceMaxConns(0); got != minDerivedMaxConns {
		t.Fatalf("16 MiB memory limit: got %d, want the floor %d", got, minDerivedMaxConns)
	}
	debug.SetMemoryLimit(math.MaxInt64)
	if got := inferenceMaxConns(0); got != noMemLimitMaxConns {
		t.Fatalf("no memory limit: got %d, want %d", got, noMemLimitMaxConns)
	}
}
