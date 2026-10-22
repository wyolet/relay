package inference

// Proxy-mode bodies over a real http.Server: deadlines, full duplex and the listener's body limit only exist on a live connection.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// proxyEdge serves an authed proxy route behind the listener's body limit, as boot wires it, with one lifecycle registry holding a one-slot admission and a post-flight watcher.
func proxyEdge(t *testing.T, upstreamURL string, limit int64, idle time.Duration) (addr string, seen <-chan postFlightSeen, adm *httpapi.Admission, store *kvtest.Recording) {
	t.Helper()
	d, pr, store := proxyStreamDeps(t, upstreamURL)
	ch := make(chan postFlightSeen, 8)
	reg := lifecycle.New()
	adm = httpapi.NewAdmission(1)
	reg.RegisterPreFlight(adm.PreFlight)
	reg.RegisterCollector(adm)
	reg.RegisterHook(lifecycle.HookFunc{HookName: "seen", Fn: func(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		ch <- postFlightSeen{lc.RequestBody, lc.RequestBodyTruncated, lc.RequestBytes, lc.RequestedModel, ev.ErrorKind}
		return nil, nil
	}})
	d.Lifecycle, d.Proxy.Lifecycle = reg, reg
	d.MaxRequestBytes, d.ProxyBodyIdle = limit, idle
	spec := d.Specs.Spec(adapters.Anthropic)
	srv := httptest.NewServer(httpmw.LimitBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithClassification(r.Context(), Classification{Mode: ModeProxyAuthed, Key: "relay-key", UpstreamAuth: "Bearer upstream-key", ClientIP: "203.0.113.9"})
		handleShape(spec, d, w, r.WithContext(withPrincipal(ctx, d.Catalog.Current(), pr, pr.Key)))
	})))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String(), ch, adm, store
}

// rawPost opens a connection and sends the request head for a body of length bytes (-1: chunked is not used, the length is declared as given). The caller writes the body.
func rawPost(t *testing.T, addr string, length int) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	fmt.Fprintf(conn, "POST /anthropic/v1/messages HTTP/1.1\r\nHost: relay\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", length)
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

// A client that stops sending mid-body is cut off with 408; its in-flight slot and reservation are released and the request is recorded as the timeout.
func TestProxyEdge_StalledBodyIsCutOff(t *testing.T) {
	up, got := recordingUpstream(t)
	addr, seen, adm, store := proxyEdge(t, up.URL, 32<<20, 200*time.Millisecond)

	body := proxyTestBody("test-model", 64<<10, false)
	conn, br := rawPost(t, addr, len(body))
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
	if s := awaitPostFlight(t, seen); s.errorKind != "request_timeout" {
		t.Fatalf("recorded as %q", s.errorKind)
	}
	if adm.Full() {
		t.Fatal("the in-flight slot is still held")
	}
	assertCancelledSettle(t, store)
	if g := <-got; g.readErr == nil {
		t.Fatal("upstream read a complete body")
	}
}

// Past the listener's limit the forward is aborted and the caller gets 413, answered on the handler's goroutine.
func TestProxyEdge_OverTheBodyLimit(t *testing.T) {
	up, got := recordingUpstream(t)
	addr, seen, _, store := proxyEdge(t, up.URL, 256<<10, 0)

	body := proxyTestBody("test-model", 1<<20, false)
	conn, br := rawPost(t, addr, len(body))
	go func() { _, _ = conn.Write(body) }()
	res, msg := readStatus(t, br)
	if res.StatusCode != http.StatusRequestEntityTooLarge || !strings.Contains(msg, "request_too_large") {
		t.Fatalf("status %d %s, want 413 request_too_large", res.StatusCode, msg)
	}
	if s := awaitPostFlight(t, seen); s.errorKind != "request_too_large" {
		t.Fatalf("recorded as %q", s.errorKind)
	}
	assertCancelledSettle(t, store)
	if g := <-got; g.readErr == nil {
		t.Fatal("upstream read a complete body")
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

// An upstream answer forwarded before the body was refused stays the caller's response, but the request is recorded as the refusal and its reservation settled as cancelled, not as a success.
func TestProxyEdge_RefusedAfterAnEarlyAnswer(t *testing.T) {
	addr, seen, _, store := proxyEdge(t, earlyUpstream(t).URL, 32<<20, 0)

	head := proxyTestBody("test-model", 64<<10, false)
	head = head[:len(head)-1] // the closing brace comes with the duplicate
	tail := []byte(`,"model":"other"}`)
	conn, br := rawPost(t, addr, len(head)+len(tail))
	if _, err := conn.Write(head); err != nil {
		t.Fatal(err)
	}
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("early answer: %v, %v", res, err)
	}
	if _, err := conn.Write(tail); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	if s := awaitPostFlight(t, seen); s.errorKind != "duplicate_model" {
		t.Fatalf("recorded as %q, want duplicate_model", s.errorKind)
	}
	assertCancelledSettle(t, store)
}

// The body limit trips on the transport's goroutine while the handler forwards the early answer's headers; under -race this is where marking the response from the limit would show.
func TestProxyEdge_OverTheLimitAfterAnEarlyAnswer(t *testing.T) {
	addr, seen, _, store := proxyEdge(t, earlyUpstream(t).URL, 256<<10, 0)

	body := proxyTestBody("test-model", 1<<20, false)
	conn, br := rawPost(t, addr, len(body))
	go func() { _, _ = conn.Write(body) }()
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	if s := awaitPostFlight(t, seen); s.errorKind != "request_too_large" {
		t.Fatalf("status %d, recorded as %q, want request_too_large", res.StatusCode, s.errorKind)
	}
	assertCancelledSettle(t, store)
}
