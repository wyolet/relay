package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	sdkusage "github.com/wyolet/relay/sdk/usage"
)

// The upstream gets the caller's method, path, headers and body verbatim under the captured credential; the caller gets the upstream's answer verbatim.
func TestRun_ForwardsRequestAndAnswerVerbatim(t *testing.T) {
	type seen struct {
		Method, URI string
		Header      http.Header
		Body        string
	}
	var got seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.RequestURI, r.Header.Clone(), string(b)}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "u1")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	p := New(nil, nil, nil)
	p.Client = upstream.Client()
	const body = `{"model":"gpt-5.5","input":"hi"}`
	req := &Request{
		Method:        http.MethodPut,
		Path:          "/v1/responses?x=1",
		Body:          strings.NewReader(body),
		ContentLength: int64(len(body)),
		Headers: http.Header{
			"Authorization":   {"Bearer stale-forwarded-header"},
			"Content-Type":    {"application/json"},
			"X-Caller-Header": {"a", "b"},
		},
		// A trailing slash on the base URL must not double up with the path's.
		HostBaseURL:  upstream.URL + "//",
		UpstreamAuth: "Bearer upstream-token",
		Lifecycle:    lifecycle.NewContext("req-test", "proxy", time.Now()),
	}
	res, err := p.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	answer, _ := io.ReadAll(res.Body)
	if err := res.Body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	want := seen{
		Method: http.MethodPut,
		URI:    "/v1/responses?x=1",
		Header: http.Header{
			"Accept-Encoding": {"gzip"},
			"Authorization":   {"Bearer upstream-token"},
			"Content-Length":  {"32"},
			"Content-Type":    {"application/json"},
			"User-Agent":      {"Go-http-client/1.1"},
			"X-Caller-Header": {"a", "b"},
		},
		Body: body,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("upstream request (-want +got):\n%s", diff)
	}
	if res.Status != http.StatusAccepted || string(answer) != `{"ok":true}` || res.Headers.Get("X-Upstream") != "u1" {
		t.Errorf("result = %d %q %v, want the upstream's 202 answer", res.Status, answer, res.Headers)
	}
	if req.Body != nil {
		t.Error("Run kept the request body reachable after the upstream answered")
	}
}

// A request without the upstream credential fails before anything is sent, and the failure is recorded.
func TestRun_NoUpstreamAuth(t *testing.T) {
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer upstream.Close()

	events := make(chan lifecycle.PostFlightEvent, 1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "test", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		events <- *ev
		return nil, nil
	}})
	res, err := New(nil, reg, nil).Run(context.Background(), &Request{
		Method: http.MethodPost, Path: "/v1/x", Body: strings.NewReader(`{}`), HostBaseURL: upstream.URL,
		Lifecycle: lifecycle.NewContext("req", "proxy", time.Now()),
	})
	if !errors.Is(err, ErrNoUpstreamAuth) || res != nil {
		t.Fatalf("Run = %v, %v; want ErrNoUpstreamAuth", res, err)
	}
	if hits != 0 {
		t.Error("a request without a credential reached the upstream")
	}
	select {
	case ev := <-events:
		want := lifecycle.PostFlightEvent{ErrorKind: "no_upstream_auth", ErrorMessage: "proxy: upstream authorization required"}
		if diff := cmp.Diff(want, ev); diff != "" {
			t.Errorf("failure event (-want +got):\n%s", diff)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no failure event")
	}
}

func TestRun_UpstreamFailuresAreWrapped(t *testing.T) {
	for name, tc := range map[string]struct{ method, baseURL, wantPrefix string }{
		"unbuildable request": {"BAD METHOD", "http://127.0.0.1:1", "proxy: build upstream request: "},
		"nothing listening":   {http.MethodPost, "http://127.0.0.1:1", "proxy: upstream call: "},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := New(nil, nil, nil).Run(context.Background(), &Request{
				Method: tc.method, Path: "/v1/x", Body: strings.NewReader(`{}`), HostBaseURL: tc.baseURL, UpstreamAuth: "Bearer k",
			})
			if res != nil || err == nil || !strings.HasPrefix(err.Error(), tc.wantPrefix) {
				t.Fatalf("Run = %v, %v; want an error starting %q", res, err, tc.wantPrefix)
			}
			if kind, status := classifyFailure(err); kind != "upstream_error" || status != 0 {
				t.Errorf("classifyFailure = %q, %d; want upstream_error, 0", kind, status)
			}
		})
	}
}

func TestClassifyFailure(t *testing.T) {
	bodyErr := &BodyError{Kind: "read_body", Err: io.ErrUnexpectedEOF}
	if bodyErr.Error() != "unexpected EOF" || !errors.Is(bodyErr, io.ErrUnexpectedEOF) {
		t.Fatalf("BodyError hides its cause: %v", bodyErr)
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"body error under a transport error", errors.Join(errors.New("proxy: upstream call"), bodyErr), "read_body"},
		{"body error beats the cancellation it caused", fmt.Errorf("%w: %w", context.Canceled, bodyErr), "read_body"},
		{"no upstream auth", ErrNoUpstreamAuth, "no_upstream_auth"},
		{"refused by a limit", fmt.Errorf("admit: %w", &limits.Refused{}), "rate_limited"},
		// A hold the store could not answer wraps the store's timeout; it must still read as a kv refusal.
		{"store unavailable wrapping a timeout", fmt.Errorf("%w: %w", pkgratelimit.ErrUnavailable, fmt.Errorf("%w: %w", kv.ErrUnavailable, context.DeadlineExceeded)), "kv_unavailable"},
		{"limiter missing", ErrLimiterMissing, "limiter_missing"},
		{"client canceled", fmt.Errorf("proxy: upstream call: %w", context.Canceled), "client_canceled"},
		{"timeout", fmt.Errorf("proxy: upstream call: %w", context.DeadlineExceeded), "timeout"},
		{"anything else", errors.New("dial tcp: refused"), "upstream_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if kind, status := classifyFailure(tt.err); kind != tt.want || status != 0 {
				t.Errorf("classifyFailure = %q, %d; want %q, 0", kind, status, tt.want)
			}
		})
	}
}

type recordingExtractor struct {
	saw  chan []byte
	toks sdkusage.Tokens
}

func (r *recordingExtractor) ExtractTokens(body []byte) sdkusage.Tokens {
	r.saw <- body
	return r.toks
}

// The proxy forwards Accept-Encoding verbatim, so the caller receives the body as the upstream compressed it, while post-flight consumers see it decoded.
func TestRunPostFlightDecodesCompressedUpstreamBody(t *testing.T) {
	const plaintext = `{"usage":{"prompt_tokens":8,"completion_tokens":16}}`
	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	_, _ = gw.Write([]byte(plaintext))
	_ = gw.Close()

	for name, tc := range map[string]struct {
		encoding string
		wire     []byte
		wantSeen string
	}{
		"gzip": {"gzip", gz.Bytes(), plaintext},
		// A body that does not decode under its declared coding is kept raw rather than dropped.
		"undecodable": {"gzip", []byte("not gzip"), "not gzip"},
	} {
		t.Run(name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", tc.encoding)
				_, _ = w.Write(tc.wire)
			}))
			defer upstream.Close()

			p := New(nil, nil, nil)
			p.Client = upstream.Client()
			ex := &recordingExtractor{saw: make(chan []byte, 1)}
			res, err := p.Run(context.Background(), &Request{
				Method: http.MethodPost,
				Path:   "/v1/chat/completions",
				Body:   strings.NewReader(`{}`),
				// An explicit Accept-Encoding stops the client from decompressing on its own.
				Headers:      http.Header{"Accept-Encoding": {"gzip"}},
				HostBaseURL:  upstream.URL,
				UpstreamAuth: "Bearer upstream-token",
				Extractor:    ex,
				Lifecycle:    lifecycle.NewContext("req-gz", "proxy", time.Now()),
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			wire, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()

			if !bytes.Equal(wire, tc.wire) {
				t.Fatalf("caller received %q, want the upstream's bytes verbatim", wire)
			}
			select {
			case saw := <-ex.saw:
				if string(saw) != tc.wantSeen {
					t.Fatalf("extractor saw %q, want %q", saw, tc.wantSeen)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("post-flight extractor never ran")
			}
		})
	}
}
