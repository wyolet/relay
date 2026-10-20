package reqbody

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
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
	req, _ := http.NewRequest(http.MethodPost, "http://upstream.invalid/", nil)
	release := Attach(req, []byte("payload"))
	if req.ContentLength != 7 {
		t.Fatalf("ContentLength = %d, want 7", req.ContentLength)
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(rc); string(got) != "payload" {
		t.Fatalf("GetBody read %q", got)
	}
	release()
	if _, err := req.GetBody(); !errors.Is(err, errReleased) {
		t.Fatalf("GetBody after release = %v, want errReleased", err)
	}
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
