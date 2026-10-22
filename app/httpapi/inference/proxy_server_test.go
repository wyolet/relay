package inference

// Proxy-mode bodies over a real http.Server: deadlines, the listener's body limit and what the server makes of an unread body only exist on a live connection.

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/lifecycle"
)

type edgeOptions struct {
	limit     int64             // listener body limit; 0 = 32 MiB
	idle      time.Duration     // RELAY_PROXY_BODY_IDLE_S
	perHour   int64             // policy requests per hour; 0 = unlimited
	transport http.RoundTripper // upstream transport; nil = the proxy's default
}

type edge struct {
	addr     string
	seen     <-chan postFlightSeen
	adm      *httpapi.Admission
	store    *kvtest.Recording
	returned <-chan struct{} // one value per proxy handler that returned
}

// halfDuplexOnly fails the test if the handler turns on full duplex: it stops the HTTP/1 server from handling an unread body itself (and golang/go#68560), which the proxy relies on. Other ResponseController calls reach the server's writer through Unwrap.
type halfDuplexOnly struct {
	http.ResponseWriter
	t *testing.T
}

func (w halfDuplexOnly) EnableFullDuplex() error {
	w.t.Error("proxy mode enabled full duplex on the inbound response")
	return nil
}

func (w halfDuplexOnly) Flush()                      { w.ResponseWriter.(http.Flusher).Flush() }
func (w halfDuplexOnly) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serverLog fails the test when the server logged a panic it recovered (net/http keeps serving, so nothing else shows it).
type serverLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *serverLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// proxyEdge serves an authed proxy route behind a chi router with the listener's body limit, as boot wires it, plus /healthz (answering 200 only on a live request context) and /smuggled (which must never be reached). One lifecycle registry holds a one-slot admission and a post-flight watcher. A panic the server recovers fails the test.
func proxyEdge(t *testing.T, upstreamURL string, o edgeOptions) edge {
	t.Helper()
	if o.limit == 0 {
		o.limit = 32 << 20
	}
	if o.perHour == 0 {
		o.perHour = 1 << 40
	}
	d, pr, store := proxyStreamDepsLimited(t, upstreamURL, o.perHour)
	ch := make(chan postFlightSeen, 8)
	reg := lifecycle.New()
	adm := httpapi.NewAdmission(1)
	reg.RegisterPreFlight(adm.PreFlight)
	reg.RegisterCollector(adm)
	reg.RegisterHook(lifecycle.HookFunc{HookName: "seen", Fn: func(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		ch <- postFlightSeen{lc.RequestBody, lc.RequestBodyTruncated, lc.RequestBytes, lc.RequestedModel, ev.ErrorKind}
		return nil, nil
	}})
	d.Lifecycle, d.Proxy.Lifecycle = reg, reg
	d.MaxRequestBytes, d.ProxyBodyIdle = o.limit, o.idle
	if o.transport != nil {
		d.Proxy.Client = &http.Client{Transport: o.transport}
	}
	spec := d.Specs.Spec(adapters.Anthropic)
	returned := make(chan struct{}, 16)

	rt := chi.NewRouter()
	rt.Use(httpmw.LimitBody(o.limit))
	rt.Post("/anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		defer func() { returned <- struct{}{} }()
		ctx := WithClassification(r.Context(), Classification{Mode: ModeProxyAuthed, Key: "relay-key", UpstreamAuth: "Bearer upstream-key", ClientIP: "203.0.113.9"})
		handleShape(spec, d, halfDuplexOnly{w, t}, r.WithContext(withPrincipal(ctx, d.Catalog.Current(), pr, pr.Key)))
	})
	rt.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Route", "healthz")
		if r.Context().Err() != nil {
			w.WriteHeader(499)
		}
	})
	rt.HandleFunc("/smuggled", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Route", "smuggled")
	})
	logs := &serverLog{}
	t.Cleanup(func() {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		if strings.Contains(logs.buf.String(), "panic") {
			t.Errorf("the server recovered a panic:\n%s", logs.buf.String())
		}
	})
	srv := httptest.NewUnstartedServer(rt)
	srv.Config.ErrorLog = log.New(logs, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return edge{srv.Listener.Addr().String(), ch, adm, store, returned}
}

// rawPost opens a connection and sends the head of a POST declaring a body of length bytes; the caller writes the body.
func rawPost(t *testing.T, addr string, length int) (net.Conn, *bufio.Reader) {
	t.Helper()
	return rawRequest(t, addr, fmt.Sprintf("Content-Length: %d", length))
}

// rawChunked opens a connection and sends the head of a chunked POST.
func rawChunked(t *testing.T, addr string) (net.Conn, *bufio.Reader) {
	t.Helper()
	return rawRequest(t, addr, "Transfer-Encoding: chunked")
}

func rawRequest(t *testing.T, addr, framing string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(conn, "POST /anthropic/v1/messages HTTP/1.1\r\nHost: relay\r\nContent-Type: application/json\r\n%s\r\n\r\n", framing)
	return conn, bufio.NewReader(conn)
}

func readStatus(t *testing.T, br *bufio.Reader) (*http.Response, string) {
	t.Helper()
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

// assertCancelledSettle waits for the reservation's one commit, which post-flight runs after its hooks, and wants it cancelled.
func assertCancelledSettle(t *testing.T, store *kvtest.Recording) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	commits := store.Named("limit.commit")
	for len(commits) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		commits = store.Named("limit.commit")
	}
	if len(commits) != 1 || len(commits[0].Args) < 7 || commits[0].Args[6] != int64(1) {
		t.Fatalf("reservation settlement: %+v, want one cancelled commit", commits)
	}
}

// smuggled is a request line inside a body: served, it means the server parsed the body's remainder as the next request.
const smuggled = "GET /smuggled HTTP/1.1\r\nHost: relay\r\n\r\n"

// assertNextExchangeClean sends rest (what the client still owed of the first body) and a GET /healthz on the same connection. The connection must be closed, or answer /healthz alone on a live context: never a request parsed from the body.
func assertNextExchangeClean(t *testing.T, conn net.Conn, br *bufio.Reader, rest string) {
	t.Helper()
	if _, err := io.WriteString(conn, rest+"GET /healthz HTTP/1.1\r\nHost: relay\r\n\r\n"); err != nil {
		return // closed
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return // closed
	}
	_ = res.Body.Close()
	if route := res.Header.Get("X-Route"); route != "healthz" || res.StatusCode != http.StatusOK {
		t.Fatalf("next exchange answered %d from route %q, want the connection closed or a clean /healthz", res.StatusCode, route)
	}
}

// earlyUpstream answers as soon as the request arrives and then reads the body, full duplex, as an upstream that rejects on headers would.
func earlyUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
		w.(http.Flusher).Flush()
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// After every way a proxy request can be refused, what the client still sends on the connection is never served as a request, and a following request never starts on a cancelled context.
func TestProxyEdge_NextRequestAfterARefusal(t *testing.T) {
	const idle = 300 * time.Millisecond
	modelFirst := proxyTestBody("test-model", 64<<10, false)
	cases := []struct {
		name      string
		early     bool        // the upstream answers before reading the body
		keepAlive bool        // the body was read to its end, so the connection may stay open
		opts      edgeOptions // idle is always set
		status    int
		// send writes the first request and returns what the client still owes of its body.
		send func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string)
	}{
		{name: "stalled body", status: http.StatusRequestTimeout, send: func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
			part := modelFirst[:len(modelFirst)/2]
			conn, br := rawPost(t, addr, len(part)+len(smuggled))
			_, _ = conn.Write(part)
			return conn, br, smuggled
		}},
		{name: "malformed chunk", status: http.StatusBadRequest, send: func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
			conn, br := rawChunked(t, addr)
			head := `{"model":"test-model","messages":[`
			fmt.Fprintf(conn, "%x\r\n%s\r\nzz\r\n%s", len(head), head, smuggled)
			return conn, br, ""
		}},
		{name: "duplicate in the held part", status: http.StatusBadRequest, send: func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
			body := []byte(`{"model":"test-model","model":"other","messages":[]}`)
			conn, br := rawPost(t, addr, len(body)+len(smuggled))
			_, _ = conn.Write(body)
			return conn, br, smuggled
		}},
		{name: "duplicate while streaming", status: http.StatusBadRequest, send: func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
			body := append(modelFirst[:len(modelFirst)-1:len(modelFirst)-1], `,"model":"other"}`...)
			conn, br := rawPost(t, addr, len(body)+len(smuggled))
			_, _ = conn.Write(body)
			return conn, br, smuggled
		}},
		{name: "over the body limit", opts: edgeOptions{limit: 32 << 10}, status: http.StatusRequestEntityTooLarge, send: func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
			conn, br := rawPost(t, addr, len(modelFirst)+len(smuggled))
			go func() { _, _ = conn.Write(modelFirst) }()
			return conn, br, smuggled
		}},
		// A remainder under the server's 256 KiB drain allowance: with full duplex on, the handler returning with it unread panics in net/http (golang/go#68560).
		{name: "model not found after the hold", status: http.StatusNotFound, send: func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
			body := proxyTestBody("no-such-model", 128<<10, false)
			conn, br := rawPost(t, addr, len(body)+len(smuggled))
			_, _ = conn.Write(body)
			return conn, br, smuggled
		}},
		{name: "rate limited", opts: edgeOptions{perHour: 1}, status: http.StatusTooManyRequests, send: func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
			spend, sbr := rawPost(t, addr, len(modelFirst))
			_, _ = spend.Write(modelFirst)
			if res, msg := readStatus(t, sbr); res.StatusCode != http.StatusOK {
				t.Fatalf("spending the limit: %d %s", res.StatusCode, msg)
			}
			conn, br := rawPost(t, addr, len(modelFirst)+len(smuggled))
			_, _ = conn.Write(modelFirst)
			return conn, br, smuggled
		}},
		// The early answer goes out while the body is unfinished, on a connection that then closes.
		{name: "early answer, body unfinished", early: true, status: http.StatusOK, send: func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
			conn, br := rawPost(t, addr, len(modelFirst)+len(smuggled))
			_, _ = conn.Write(modelFirst)
			return conn, br, smuggled
		}},
		{name: "early answer, body complete", early: true, keepAlive: true, status: http.StatusOK, send: func(t *testing.T, addr string) (net.Conn, *bufio.Reader, string) {
			conn, br := rawPost(t, addr, len(modelFirst))
			_, _ = conn.Write(modelFirst)
			return conn, br, ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var upstream string
			if tc.early {
				upstream = earlyUpstream(t).URL
			} else {
				up, _ := recordingUpstream(t)
				upstream = up.URL
			}
			tc.opts.idle = idle
			e := proxyEdge(t, upstream, tc.opts)
			conn, br, rest := tc.send(t, e.addr)
			res, msg := readStatus(t, br)
			if res.StatusCode != tc.status {
				t.Fatalf("first response %d %s, want %d", res.StatusCode, msg, tc.status)
			}
			if !tc.keepAlive && !res.Close {
				t.Fatalf("an answer with the body unread left the connection open (%d %s)", res.StatusCode, msg)
			}
			assertNextExchangeClean(t, conn, br, rest)
		})
	}
}

// A client that stops sending mid-body is cut off with 408; its in-flight slot and reservation are released and the request is recorded as the timeout.
func TestProxyEdge_StalledBodyIsCutOff(t *testing.T) {
	up, got := recordingUpstream(t)
	e := proxyEdge(t, up.URL, edgeOptions{idle: 200 * time.Millisecond})

	body := proxyTestBody("test-model", 64<<10, false)
	conn, br := rawPost(t, e.addr, len(body))
	if _, err := conn.Write(body[:len(body)/2]); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res, msg := readStatus(t, br)
	if res.StatusCode != http.StatusRequestTimeout || !strings.Contains(msg, "request_timeout") {
		t.Fatalf("status %d %s, want 408 request_timeout", res.StatusCode, msg)
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Fatalf("cut off after %v", waited)
	}
	if s := awaitPostFlight(t, e.seen); s.errorKind != "request_timeout" {
		t.Fatalf("recorded as %q", s.errorKind)
	}
	if e.adm.Full() {
		t.Fatal("the in-flight slot is still held")
	}
	assertCancelledSettle(t, e.store)
	if g := <-got; g.readErr == nil {
		t.Fatal("upstream read a complete body")
	}
}

// Past the listener's limit the forward is aborted and the caller gets 413, answered on the handler's goroutine.
func TestProxyEdge_OverTheBodyLimit(t *testing.T) {
	up, got := recordingUpstream(t)
	e := proxyEdge(t, up.URL, edgeOptions{limit: 256 << 10})

	body := proxyTestBody("test-model", 1<<20, false)
	conn, br := rawPost(t, e.addr, len(body))
	go func() { _, _ = conn.Write(body) }()
	res, msg := readStatus(t, br)
	if res.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(msg, "request_too_large") {
		t.Fatalf("status %d %s, want 413 request_too_large", res.StatusCode, msg)
	}
	if s := awaitPostFlight(t, e.seen); s.errorKind != "request_too_large" {
		t.Fatalf("recorded as %q", s.errorKind)
	}
	assertCancelledSettle(t, e.store)
	if g := <-got; g.readErr == nil {
		t.Fatal("upstream read a complete body")
	}
}

// A body refused after an early upstream answer: the caller gets the refusal if it came before the answer was forwarded, else the answer on a connection that then closes. Either way the request is recorded as the refusal with its reservation settled as cancelled.
func TestProxyEdge_RefusedAfterAnEarlyAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int64
		body   []byte
		status int
		kind   string
	}{
		{"duplicate after an early answer", 0, append(proxyTestBody("test-model", 64<<10, false)[:64<<10:64<<10], `"}],"model":"other"}`...), http.StatusBadRequest, "duplicate_model"},
		{"over the limit after an early answer", 256 << 10, proxyTestBody("test-model", 1<<20, false), http.StatusRequestEntityTooLarge, "request_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := proxyEdge(t, earlyUpstream(t).URL, edgeOptions{limit: tc.limit})
			conn, br := rawPost(t, e.addr, len(tc.body))
			go func() { _, _ = conn.Write(tc.body) }()
			res, msg := readStatus(t, br)
			if (res.StatusCode != tc.status && res.StatusCode != http.StatusOK) || !res.Close {
				t.Fatalf("status %d (close %v) %s, want %d or the early 200, with the connection closed", res.StatusCode, res.Close, msg, tc.status)
			}
			if s := awaitPostFlight(t, e.seen); s.errorKind != tc.kind {
				t.Fatalf("recorded as %q, want %s", s.errorKind, tc.kind)
			}
			assertCancelledSettle(t, e.store)
		})
	}
}
