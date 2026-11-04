package connadmit

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

var testBody = []byte(`{"error":{"type":"server_error","code":"overloaded","message":"busy"}}`)

func listen(t *testing.T, maxConns, queue int, wait time.Duration) *Listener {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := New(inner, Config{MaxConns: maxConns, QueueSize: queue, QueueTimeout: wait, Body: testBody, RetryAfter: "1", Header: http.Header{"X-Test": {"yes"}}})
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// counts reads the listener's slots held, queued conns and idle conns.
func (l *Listener) counts() (held, queued, idle int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held, l.qLen, l.idleLen
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitQueued(t *testing.T, l *Listener, n int) {
	t.Helper()
	waitFor(t, "queued conns", func() bool { _, q, _ := l.counts(); return q == n })
}

// acceptInto runs Accept in the background and delivers each conn on the returned channel.
func acceptInto(l *Listener) <-chan net.Conn {
	ch := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				close(ch)
				return
			}
			ch <- c
		}
	}()
	return ch
}

func expectAccepted(t *testing.T, ch <-chan net.Conn) net.Conn {
	t.Helper()
	select {
	case c := <-ch:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("no connection accepted")
		return nil
	}
}

func expectNotAccepted(t *testing.T, ch <-chan net.Conn, wait time.Duration) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("a connection past the cap was accepted")
	case <-time.After(wait):
	}
}

func dial(t *testing.T, l *Listener) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

const getRequest = "GET / HTTP/1.1\r\nHost: x\r\n\r\n"

// expectRefusal sends a request on c and checks it gets the whole 503 and then a clean close, not a reset.
func expectRefusal(t *testing.T, c net.Conn) {
	t.Helper()
	if _, err := io.WriteString(c, getRequest); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read refusal: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read refusal body: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || !resp.Close || resp.Header.Get("Retry-After") != "1" ||
		resp.Header.Get("Content-Type") != "application/json" || resp.Header.Get("X-Test") != "yes" || string(body) != string(testBody) {
		t.Fatalf("refusal = %d close=%v headers=%v body=%q", resp.StatusCode, resp.Close, resp.Header, body)
	}
	if _, err := br.ReadByte(); err != io.EOF {
		t.Fatalf("after the refusal: %v, want EOF", err)
	}
}

func TestServesUpToCap(t *testing.T) {
	l := listen(t, 2, 4, 5*time.Second)
	accepted := acceptInto(l)
	dial(t, l)
	dial(t, l)
	expectAccepted(t, accepted)
	expectAccepted(t, accepted)
	dial(t, l)
	waitQueued(t, l, 1)
	expectNotAccepted(t, accepted, 100*time.Millisecond)
}

func TestQueuedConnServedWhenSlotFrees(t *testing.T) {
	l := listen(t, 1, 1, 5*time.Second)
	accepted := acceptInto(l)
	dial(t, l)
	first := expectAccepted(t, accepted)
	dial(t, l)
	waitQueued(t, l, 1)
	expectNotAccepted(t, accepted, 100*time.Millisecond)
	_ = first.Close()
	expectAccepted(t, accepted)
	if held, queued, _ := l.counts(); held != 1 || queued != 0 {
		t.Fatalf("held, queued = %d, %d; want 1, 0", held, queued)
	}
}

func TestQueueFullAnswersRefusal(t *testing.T) {
	l := listen(t, 1, 1, 5*time.Second)
	accepted := acceptInto(l)
	dial(t, l)
	expectAccepted(t, accepted)
	dial(t, l)
	waitQueued(t, l, 1)
	before := testutil.ToFloat64(refusedQueueFull)
	expectRefusal(t, dial(t, l))
	if got := testutil.ToFloat64(refusedQueueFull) - before; got != 1 {
		t.Fatalf("queue_full refusals = %v, want 1", got)
	}
}

func TestNoQueueRefusesPastCap(t *testing.T) {
	l := listen(t, 1, 0, time.Second)
	accepted := acceptInto(l)
	dial(t, l)
	expectAccepted(t, accepted)
	expectRefusal(t, dial(t, l))
}

func TestQueueWaitExpiryAnswersRefusal(t *testing.T) {
	l := listen(t, 1, 1, 200*time.Millisecond)
	accepted := acceptInto(l)
	dial(t, l)
	expectAccepted(t, accepted)
	before := testutil.ToFloat64(refusedQueueTimeout)
	start := time.Now()
	expectRefusal(t, dial(t, l))
	if waited := time.Since(start); waited < 150*time.Millisecond {
		t.Fatalf("refused after %v, before the queue wait", waited)
	}
	if got := testutil.ToFloat64(refusedQueueTimeout) - before; got != 1 {
		t.Fatalf("queue_timeout refusals = %v, want 1", got)
	}
	if _, queued, _ := l.counts(); queued != 0 {
		t.Fatalf("queued = %d after expiry", queued)
	}
}

// A freed slot skips a queued conn whose wait is over and goes to the next one.
func TestReleaseSkipsExpiredQueuedConn(t *testing.T) {
	l := listen(t, 1, 2, time.Second)
	accepted := acceptInto(l)
	dial(t, l)
	first := expectAccepted(t, accepted)
	expired := dial(t, l)
	waitQueued(t, l, 1)
	// Stop the expiry timer so only the release sees the deadline.
	l.mu.Lock()
	l.timer.Stop()
	l.mu.Unlock()
	time.Sleep(500 * time.Millisecond)
	dial(t, l)
	waitQueued(t, l, 2)
	time.Sleep(600 * time.Millisecond)
	_ = first.Close()
	expectAccepted(t, accepted)
	expectRefusal(t, expired)
}

func TestDoubleCloseReleasesOnce(t *testing.T) {
	l := listen(t, 1, 2, 5*time.Second)
	accepted := acceptInto(l)
	dial(t, l)
	first := expectAccepted(t, accepted)
	dial(t, l)
	dial(t, l)
	waitQueued(t, l, 2)
	_ = first.Close()
	_ = first.Close()
	expectAccepted(t, accepted)
	expectNotAccepted(t, accepted, 100*time.Millisecond)
	if held, queued, _ := l.counts(); held != 1 || queued != 1 {
		t.Fatalf("held, queued = %d, %d; want 1, 1", held, queued)
	}
}

func TestCloseClosesQueuedConns(t *testing.T) {
	l := listen(t, 1, 1, 5*time.Second)
	dial(t, l)
	served, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer served.Close()
	waiting := dial(t, l)
	waitQueued(t, l, 1)

	acceptErr := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		acceptErr <- err
	}()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-acceptErr:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Accept after Close = %v, want net.ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Accept did not return after Close")
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("second Accept after Close = %v", err)
	}
	_ = waiting.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := waiting.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("queued conn read after Close = %v, want it closed", err)
	}
}

// Conns handed out but not yet accepted when the listener closes are closed with it.
func TestCloseClosesConnsNotYetAccepted(t *testing.T) {
	l := listen(t, 2, 0, time.Second)
	c := dial(t, l)
	waitFor(t, "slot taken", func() bool { held, _, _ := l.counts(); return held == 1 })
	_ = l.Close()
	waitFor(t, "slot freed", func() bool { held, _, _ := l.counts(); return held == 0 })
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("conn not accepted before Close: read = %v, want it closed", err)
	}
}
