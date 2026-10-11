package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

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
			bound, errs := serve(srv)
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
	bound, errs := serve(&http.Server{Addr: ln.Addr().String()})
	if bound != nil {
		t.Fatalf("serve bound %v on a taken address", bound)
	}
	if err := <-errs; err == nil {
		t.Fatal("no bind error reported")
	}
}
