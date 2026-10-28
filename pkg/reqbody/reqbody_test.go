package reqbody

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestReaderReadsAllThenDropsBuffer(t *testing.T) {
	r := NewReader([]byte("hello world")).(*reader)
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "hello world" {
		t.Fatalf("ReadAll = %q, %v", got, err)
	}
	if r.b != nil {
		t.Fatal("buffer still referenced after EOF")
	}
}

func TestReaderCloseDropsBufferAndFailsLaterReads(t *testing.T) {
	r := NewReader([]byte("hello")).(*reader)
	_ = r.Close()
	if r.b != nil {
		t.Fatal("buffer still referenced after Close")
	}
	if _, err := r.Read(make([]byte, 4)); !errors.Is(err, errClosed) {
		t.Fatalf("Read after Close = %v, want errClosed", err)
	}
}

func TestAttachSetsLengthAndRewindsUntilReleased(t *testing.T) {
	body := bytes.Repeat([]byte("x"), pinnedMax+1)
	req, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/", nil)
	release := Attach(req, body)
	if req.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength = %d, want %d", req.ContentLength, len(body))
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, body) {
		t.Fatalf("GetBody read %d bytes, want %d", len(got), len(body))
	}
	release()
	if _, err := req.GetBody(); !errors.Is(err, errReleased) {
		t.Fatalf("GetBody after release = %v, want errReleased", err)
	}
}

func TestAttachSmallBodyMatchesNewRequest(t *testing.T) {
	body := []byte("payload")
	req, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/", nil)
	release := Attach(req, body)
	want, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/", bytes.NewReader(body))
	if req.ContentLength != want.ContentLength {
		t.Fatalf("ContentLength = %d, want %d", req.ContentLength, want.ContentLength)
	}
	release()
	for range 2 {
		rc, err := req.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := io.ReadAll(rc); string(got) != "payload" {
			t.Fatalf("GetBody read %q", got)
		}
	}
	if got, _ := io.ReadAll(req.Body); string(got) != "payload" {
		t.Fatalf("Body read %q", got)
	}
}

// net/http flushes the headers on their own unless it knows the body is in memory.
func TestAttachSmallBodyWritesOnce(t *testing.T) {
	gotBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody <- b
	}))
	defer srv.Close()

	var writes atomic.Int32
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return writeCountingConn{Conn: c, writes: &writes}, nil
		},
	}}
	defer client.CloseIdleConnections()

	body := bytes.Repeat([]byte("x"), 3<<10)
	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	release := Attach(req, body)
	resp, err := client.Do(req)
	release()
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-gotBody; !bytes.Equal(got, body) {
		t.Fatalf("upstream read %d bytes, want %d", len(got), len(body))
	}
	if n := writes.Load(); n != 1 {
		t.Fatalf("request took %d writes, want 1", n)
	}
}

type writeCountingConn struct {
	net.Conn
	writes *atomic.Int32
}

func (c writeCountingConn) Write(p []byte) (int, error) {
	c.writes.Add(1)
	return c.Conn.Write(p)
}

func TestAttachEmptyBodyIsNoBody(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/", nil)
	Attach(req, nil)
	if req.Body != http.NoBody || req.ContentLength != 0 {
		t.Fatalf("Body = %v, ContentLength = %d", req.Body, req.ContentLength)
	}
}

// The transport keeps the request reachable while the response is read; the attached buffer must not be.
func TestAttachedBufferCollectableWhileResponseStreams(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer srv.Close()
	defer close(release)

	resp, collected := sendLargeBody(t, srv.URL)
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, len("first"))); err != nil {
		t.Fatal(err)
	}
	if !waitCollected(collected) {
		t.Fatal("request buffer still reachable while the response is open")
	}
}

func sendLargeBody(t *testing.T, url string) (*http.Response, <-chan struct{}) {
	t.Helper()
	body := bytes.Repeat([]byte("x"), 1<<20)
	collected := make(chan struct{})
	runtime.AddCleanup(&body[0], func(ch chan struct{}) { close(ch) }, collected)
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	releaseBody := Attach(req, body)
	resp, err := http.DefaultClient.Do(req)
	releaseBody()
	if err != nil {
		t.Fatal(err)
	}
	return resp, collected
}

func waitCollected(done <-chan struct{}) bool {
	for range 100 {
		runtime.GC()
		select {
		case <-done:
			return true
		case <-time.After(10 * time.Millisecond):
		}
	}
	return false
}
