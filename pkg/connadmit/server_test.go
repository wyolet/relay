package connadmit

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// startServer serves h on l with the idle-tracking hook.
func startServer(t *testing.T, l *Listener, h http.Handler) {
	t.Helper()
	srv := &http.Server{Handler: h, ConnState: l.ConnState, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
}

var noContent = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

// get sends a keep-alive request on c and reads the response.
func get(c net.Conn, br *bufio.Reader, wait time.Duration) (*http.Response, error) {
	if _, err := io.WriteString(c, getRequest); err != nil {
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(wait))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp, nil
}

type client struct {
	c  net.Conn
	br *bufio.Reader
}

func newClient(t *testing.T, l *Listener) client {
	c := dial(t, l)
	return client{c: c, br: bufio.NewReader(c)}
}

func (cl client) mustGet(t *testing.T) {
	t.Helper()
	resp, err := get(cl.c, cl.br, 3*time.Second)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestOldestIdleConnClosedForQueued(t *testing.T) {
	l := listen(t, 2, 4, 5*time.Second)
	startServer(t, l, noContent)
	before := testutil.ToFloat64(idleClosed)

	old := newClient(t, l)
	old.mustGet(t)
	time.Sleep(minIdleAge + 100*time.Millisecond)
	recent := newClient(t, l)
	recent.mustGet(t)
	waitFor(t, "two idle conns", func() bool { _, _, idle := l.counts(); return idle == 2 })

	queued := newClient(t, l)
	queued.mustGet(t)
	_ = old.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := old.br.ReadByte(); err != io.EOF {
		t.Fatalf("oldest idle conn read = %v, want EOF", err)
	}
	recent.mustGet(t)
	if got := testutil.ToFloat64(idleClosed) - before; got != 1 {
		t.Fatalf("idle closed = %v, want 1", got)
	}

	// Both held conns have been idle for less than the minimum age, so the next conn waits.
	waitFor(t, "two idle conns", func() bool { _, _, idle := l.counts(); return idle == 2 })
	late := newClient(t, l)
	if _, err := get(late.c, late.br, 200*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("conn past the cap with only recently idle conns: %v, want it to wait", err)
	}
	if got := testutil.ToFloat64(idleClosed) - before; got != 1 {
		t.Fatalf("idle closed = %v, want 1", got)
	}
}

func TestIdleConnKeptWithoutCloseIdle(t *testing.T) {
	l := listen(t, 1, 1, 5*time.Second)
	l.closeIdle = false
	startServer(t, l, noContent)
	before := testutil.ToFloat64(idleClosed)

	old := newClient(t, l)
	old.mustGet(t)
	time.Sleep(minIdleAge + 100*time.Millisecond)
	waiting := newClient(t, l)
	if _, err := get(waiting.c, waiting.br, 200*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("conn past the cap: %v, want it to wait", err)
	}
	old.mustGet(t)
	if got := testutil.ToFloat64(idleClosed) - before; got != 0 {
		t.Fatalf("idle closed = %v, want 0", got)
	}
}

func TestHijackedConnHoldsSlot(t *testing.T) {
	l := listen(t, 1, 1, 5*time.Second)
	hijacked := make(chan net.Conn, 1)
	startServer(t, l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			c, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			hijacked <- c
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	ws := dial(t, l)
	if _, err := io.WriteString(ws, "GET /ws HTTP/1.1\r\nHost: x\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	var conn net.Conn
	select {
	case conn = <-hijacked:
	case <-time.After(3 * time.Second):
		t.Fatal("no hijack")
	}
	time.Sleep(minIdleAge + 100*time.Millisecond)

	next := newClient(t, l)
	if _, err := get(next.c, next.br, 200*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("conn while the hijacked one holds the slot: %v, want it to wait", err)
	}
	_ = conn.Close()
	_ = next.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(next.br, nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("queued conn after the hijacked one closed: %v", err)
	}
}

// Concurrent clients through a real server: every request is served or cleanly refused, and every slot comes back.
func TestServerUnderConcurrentClients(t *testing.T) {
	l := listen(t, 4, 8, 2*time.Second)
	startServer(t, l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(10 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	tr := &http.Transport{MaxIdleConnsPerHost: 2}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: 5 * time.Second}

	var mu sync.Mutex
	statuses := map[int]int{}
	var wg sync.WaitGroup
	for range 24 {
		wg.Go(func() {
			for range 5 {
				resp, err := hc.Get("http://" + l.Addr().String() + "/")
				if err != nil {
					t.Errorf("request: %v", err)
					return
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusServiceUnavailable && (string(body) != string(testBody) || resp.Header.Get("Retry-After") != "1") {
					t.Errorf("malformed refusal: %v %q", resp.Header, body)
				}
				mu.Lock()
				statuses[resp.StatusCode]++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if statuses[http.StatusNoContent] == 0 || len(statuses) > 2 || (len(statuses) == 2 && statuses[http.StatusServiceUnavailable] == 0) {
		t.Fatalf("statuses = %v", statuses)
	}
	tr.CloseIdleConnections()
	waitFor(t, "every slot freed", func() bool {
		held, queued, idle := l.counts()
		return held == 0 && queued == 0 && idle == 0
	})
}
