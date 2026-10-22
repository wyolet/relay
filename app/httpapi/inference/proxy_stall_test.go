package inference

// Forwards that stop moving, over HTTP/1 and HTTP/2 upstreams: each must end within a few idle periods, its handler returning and leaving no goroutine behind.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime/pprof"
	"sync/atomic"
	"testing"
	"time"
)

// failingConn fails every write once armed.
type failingConn struct {
	net.Conn
	armed *atomic.Bool
}

func (c failingConn) Write(p []byte) (int, error) {
	if c.armed.Load() {
		return 0, errors.New("injected write failure")
	}
	return c.Conn.Write(p)
}

// h2cUpstream serves h over HTTP/2 without TLS (and HTTP/1), the handler released when the test ends.
func h2cUpstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// h2cTransport speaks HTTP/2 without TLS over connections whose writes fail once armed is set.
func h2cTransport(armed *atomic.Bool) *http.Transport {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Transport{Protocols: p, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return failingConn{c, armed}, nil
	}}
}

// blockingUpstream starts, through serve, an upstream that holds every request until the test ends, answering first when answer is set. The release is registered after serve's own cleanup, so it runs first and the server's Close does not wait on a held handler.
func blockingUpstream(t *testing.T, answer bool, serve func(*testing.T, http.HandlerFunc) *httptest.Server) string {
	t.Helper()
	release := make(chan struct{})
	srv := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if answer {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
		}
		<-release
	})
	t.Cleanup(func() { close(release) })
	return srv.URL
}

func h1Upstream(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// forwardGoroutines lists the goroutines still in a forward: the handler, its watch, a transport writer parked on flow control.
func forwardGoroutines() []string {
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var left []string
	for _, g := range bytes.Split(buf.Bytes(), []byte("\n\n")) {
		for _, mark := range []string{"inference.handleProxy", "inference.watchForward", "awaitFlowControl", "(*scannedBody).Read"} {
			if bytes.Contains(g, []byte(mark)) {
				left = append(left, string(g))
				break
			}
		}
	}
	return left
}

func TestProxyEdge_StalledForwardsEnd(t *testing.T) {
	const idle = 200 * time.Millisecond
	cases := []struct {
		name string
		// upstream returns the upstream's URL and the transport to reach it.
		upstream func(t *testing.T) (string, http.RoundTripper)
		body     []byte
		// leave is when the client gives up and closes its connection; 0 = it waits for the answer.
		leave  time.Duration
		status int // 0 = any outcome
	}{
		{name: "h2, header write fails", status: http.StatusBadGateway, body: proxyTestBody("test-model", 1<<10, false), upstream: func(t *testing.T) (string, http.RoundTripper) {
			up := h2cUpstream(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body) })
			var armed atomic.Bool
			tr := h2cTransport(&armed)
			// A warm connection makes the next request's first write the HEADERS frame.
			res, err := (&http.Client{Transport: tr}).Get(up.URL)
			if err != nil || res.ProtoMajor != 2 {
				t.Fatalf("warm-up: %v %v", res, err)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			armed.Store(true)
			return up.URL, tr
		}},
		{name: "h2, early 200 then no window", body: proxyTestBody("test-model", 8<<20, false), upstream: func(t *testing.T) (string, http.RoundTripper) {
			var armed atomic.Bool
			return blockingUpstream(t, true, h2cUpstream), h2cTransport(&armed)
		}},
		{name: "h2, silent upstream", status: http.StatusBadGateway, body: proxyTestBody("test-model", 24<<20, false), upstream: func(t *testing.T) (string, http.RoundTripper) {
			var armed atomic.Bool
			return blockingUpstream(t, false, h2cUpstream), h2cTransport(&armed)
		}},
		{name: "h1, silent upstream", status: http.StatusBadGateway, body: proxyTestBody("test-model", 24<<20, false), upstream: func(t *testing.T) (string, http.RoundTripper) {
			return blockingUpstream(t, false, h1Upstream), &http.Transport{}
		}},
		{name: "h1, silent upstream, client gone mid-upload", leave: 300 * time.Millisecond, body: proxyTestBody("test-model", 24<<20, false), upstream: func(t *testing.T) (string, http.RoundTripper) {
			return blockingUpstream(t, false, h1Upstream), &http.Transport{}
		}},
		{name: "h2, silent upstream, client gone mid-upload", leave: 300 * time.Millisecond, body: proxyTestBody("test-model", 24<<20, false), upstream: func(t *testing.T) (string, http.RoundTripper) {
			var armed atomic.Bool
			return blockingUpstream(t, false, h2cUpstream), h2cTransport(&armed)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, tr := tc.upstream(t)
			e := proxyEdge(t, url, edgeOptions{idle: idle, transport: tr})
			conn, br := rawPost(t, e.addr, len(tc.body))
			start := time.Now()
			go func() { _, _ = conn.Write(tc.body) }()
			if tc.leave > 0 {
				time.Sleep(tc.leave)
				_ = conn.Close()
			} else if res, err := http.ReadResponse(br, nil); err == nil {
				_, _ = io.Copy(io.Discard, res.Body)
				if tc.status != 0 && res.StatusCode != tc.status {
					t.Fatalf("status %d, want %d", res.StatusCode, tc.status)
				}
			} else if tc.status != 0 {
				t.Fatalf("no answer: %v", err)
			}

			select {
			case <-e.returned:
				if took := time.Since(start); took > 4*idle+time.Second {
					t.Fatalf("the handler took %v", took)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("the handler never returned:\n%s", forwardGoroutines())
			}
			deadline := time.Now().Add(3 * time.Second)
			for left := forwardGoroutines(); len(left) > 0; left = forwardGoroutines() {
				if time.Now().After(deadline) {
					t.Fatalf("forward goroutines left behind:\n%s", left)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}
