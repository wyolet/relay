package inference

// Once the upstream answers, nothing may keep the inbound request body reachable: a long stream would otherwise pin it until the caller finishes reading.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/pkg/httpmw"
	"github.com/wyolet/relay/pkg/reqbody"
)

const releasedBodyPad = 1 << 20

// heldStreamUpstream reads the whole request, streams head, then keeps the response open until the test ends.
func heldStreamUpstream(t *testing.T, head string) *httptest.Server {
	t.Helper()
	hold := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(head))
		w.(http.Flusher).Flush()
		select {
		case <-hold:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(hold) })
	return srv
}

// firstWriteRecorder reports when the first response bytes reach the caller.
type firstWriteRecorder struct {
	*httptest.ResponseRecorder
	once  sync.Once
	wrote chan struct{}
}

func (f *firstWriteRecorder) Write(p []byte) (int, error) {
	n, err := f.ResponseRecorder.Write(p)
	f.once.Do(func() { close(f.wrote) })
	return n, err
}

// trackCollection returns b and a channel closed once b's backing array is garbage-collected.
func trackCollection(b []byte) ([]byte, <-chan struct{}) {
	collected := make(chan struct{})
	runtime.AddCleanup(&b[0], func(ch chan struct{}) { close(ch) }, collected)
	return b, collected
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

func largeStreamBody() []byte {
	return []byte(`{"model":"test-model","max_tokens":16,"messages":[{"role":"user","content":"` + strings.Repeat("a", releasedBodyPad) + `"}],"stream":true}`)
}

// dispatchHeldStream runs Dispatch on a fresh large body in its own goroutine; the body is built there so the test holds no reference to it.
func dispatchHeldStream(t *testing.T, d Deps, r *http.Request, inbound adapters.Name) (w *firstWriteRecorder, collected <-chan struct{}, done <-chan struct{}) {
	t.Helper()
	w = &firstWriteRecorder{ResponseRecorder: httptest.NewRecorder(), wrote: make(chan struct{})}
	tracked := make(chan (<-chan struct{}), 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		body, c := trackCollection(largeStreamBody())
		tracked <- c
		Dispatch(d, w, r, DispatchInput{Inbound: inbound, Body: body, ModelName: "test-model", Stream: true})
	}()
	select {
	case <-w.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("no response bytes reached the caller")
	}
	return w, <-tracked, finished
}

func TestDispatch_RequestBodyCollectableMidStream(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		inbound adapters.Name
	}{
		{"byte-pass", "/anthropic/v1/messages", adapters.Anthropic},
		{"cross-shape", "/v1/chat/completions", adapters.OpenAI},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := heldStreamUpstream(t, anthropicStreamHead)
			cat, rk := keepAliveCatalog(t, up.URL)
			d := keepAliveDeps(t, cat, 0)
			r := withNormalContext(httptest.NewRequest(http.MethodPost, tc.path, nil), rk)

			w, collected, done := dispatchHeldStream(t, d, r, tc.inbound)
			if !waitCollected(collected) {
				t.Fatal("request body still reachable while the response streams")
			}
			if w.Code != http.StatusOK {
				t.Fatalf("status %d", w.Code)
			}
			select {
			case <-done:
				t.Fatal("stream ended before the body was checked")
			default:
			}
		})
	}
}

// The proxy forward, as handleProxy wires it: the bytes held until the model was known, then the rest streamed from the client.
func TestProxyRun_RequestBodyCollectableMidStream(t *testing.T) {
	for _, pad := range []int{512 << 10, 2 << 20} {
		up := heldStreamUpstream(t, "data: {}\n\n")

		res, collected := startProxyStream(t, up.URL, pad)
		if _, err := io.ReadFull(res.Body, make([]byte, len("data: {}\n\n"))); err != nil {
			t.Fatalf("pad %d: read: %v", pad, err)
		}
		for i, c := range collected {
			if !waitCollected(c) {
				t.Fatalf("pad %d: buffer %d still reachable while the response streams", pad, i)
			}
		}
		_ = res.Body.Close()
	}
}

func TestReadRequestBody(t *testing.T) {
	const limit = 64
	payload := strings.Repeat("b", 40)
	cases := []struct {
		name          string
		body          string
		contentLength int64
		wantTooLarge  bool
	}{
		{"declared length", payload, int64(len(payload)), false},
		{"unknown length", payload, -1, false},
		{"over the limit, declared", strings.Repeat("b", limit+1), limit + 1, true},
		{"over the limit, unknown", strings.Repeat("b", limit+1), -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/generate", strings.NewReader(tc.body))
			r.ContentLength = tc.contentLength
			r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, limit)
			got, err := readRequestBody(r, limit)
			if tc.wantTooLarge {
				if !httpmw.IsBodyTooLargeError(err) {
					t.Fatalf("err = %v, want a body-too-large error", err)
				}
				return
			}
			if err != nil || string(got) != tc.body {
				t.Fatalf("read %q, %v", got, err)
			}
		})
	}
}

func TestReadRequestBody_AllocatesUpFrontOnlyToTheCap(t *testing.T) {
	read := func(body string, declared int64) ([]byte, error) {
		r := httptest.NewRequest(http.MethodPost, "/v1/generate", strings.NewReader(body))
		r.ContentLength = declared
		return readRequestBody(r, httpmw.DefaultMaxRequestBytes)
	}

	got, err := read("0123456789", httpmw.DefaultMaxRequestBytes)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short body err = %v, want io.ErrUnexpectedEOF", err)
	}
	if cap(got) > maxUpfrontBodyBytes {
		t.Fatalf("declared 32 MiB, sent 10 bytes: allocated %d, want <= %d", cap(got), maxUpfrontBodyBytes)
	}

	small := strings.Repeat("s", 4096)
	if got, err := read(small, int64(len(small))); err != nil || string(got) != small || cap(got) != len(small) {
		t.Fatalf("small body: len %d cap %d err %v, want one exact allocation", len(got), cap(got), err)
	}

	large := strings.Repeat("l", 3<<20+7)
	if got, err := read(large, int64(len(large))); err != nil || string(got) != large {
		t.Fatalf("large body: read %d bytes, err %v", len(got), err)
	}
}

func startProxyStream(t *testing.T, upstreamURL string, pad int) (*proxy.Result, []<-chan struct{}) {
	t.Helper()
	body, bodyGone := trackCollection(proxyTestBody("test-model", pad, false))
	sb, held, rej := holdProxyBody(reqbody.NewReader(body), int64(len(body)), true)
	if rej != nil {
		t.Fatalf("hold: %v", rej)
	}
	_, heldGone := trackCollection(held)
	res, err := proxy.New(nil, nil, nil).Run(context.Background(), &proxy.Request{
		Method:        http.MethodPost,
		Path:          "/v1/chat/completions",
		Body:          sb,
		ContentLength: int64(len(body)),
		Headers:       http.Header{},
		HostBaseURL:   upstreamURL,
		UpstreamAuth:  "Bearer upstream-key",
	})
	if err != nil {
		t.Fatalf("proxy run: %v", err)
	}
	return res, []<-chan struct{}{bodyGone, heldGone}
}
