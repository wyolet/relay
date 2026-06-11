package inference

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	apphost "github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/lifecycle"
)

func TestProxyUpstreamPath_DefaultsToSpecPath(t *testing.T) {
	spec := (&adapter.Spec{UpstreamPath: "/v1/responses"}).Build()

	got := proxyUpstreamPath("/openai/v1/responses", spec, nil)

	if got != "/v1/responses" {
		t.Fatalf("path: got %q", got)
	}
}

func TestProxyUpstreamPath_HostBackendOverride(t *testing.T) {
	spec := (&adapter.Spec{UpstreamPath: "/v1/responses"}).Build()
	host := &apphost.Host{
		Spec: apphost.Spec{Backend: map[string]string{"upstreamPath": "/responses"}},
	}

	got := proxyUpstreamPath("/openai/v1/responses", spec, host)

	if got != "/responses" {
		t.Fatalf("path: got %q", got)
	}
}

func TestScanTopLevelModel(t *testing.T) {
	pad := strings.Repeat("x", 4096)
	cases := []struct {
		name  string
		body  string
		want  string
		found bool
	}{
		{"model first, truncated tail", `{"model":"test-model","messages":[{"role":"user","content":"` + pad, "test-model", true},
		{"model after truncated array", `{"messages":[{"role":"user","content":"` + pad, "", false},
		{"nested model must not match", `{"messages":[{"model":"nested"}],"max_tokens":5}`, "", false},
		{"nested model then top-level", `{"messages":[{"model":"nested"}],"model":"test-model"}`, "test-model", true},
		{"no model", `{"messages":[],"stream":true}`, "", false},
		{"not an object", `[1,2,3]`, "", false},
		{"model not a string", `{"model":42,"messages":[]}`, "", false},
		{"model cut mid-string", `{"model":"test-mo`, "", false},
		{"empty input", ``, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := scanTopLevelModel([]byte(tc.body))
			if found != tc.found || got != tc.want {
				t.Fatalf("scan: got (%q, %v), want (%q, %v)", got, found, tc.want, tc.found)
			}
		})
	}
}

// proxyTestBody builds a JSON chat body around the given model with a
// padding payload, placing model first or last among the top-level keys.
func proxyTestBody(model string, padding int, modelLast bool) []byte {
	pad := strings.Repeat("a", padding)
	if modelLast {
		return []byte(`{"messages":[{"role":"user","content":"` + pad + `"}],"model":"` + model + `"}`)
	}
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"` + pad + `"}]}`)
}

func TestResolveProxyHostByPolicy_BodyLadder(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	resolver := routing.New(cat)

	cases := []struct {
		name          string
		body          []byte
		wantReason    string // "" = success
		wantTruncated bool
	}{
		{"small body", proxyTestBody("test-model", 64, false), "", false},
		{"large model first streams", proxyTestBody("test-model", 2<<20, false), "", true},
		{"large model last buffers", proxyTestBody("test-model", 2<<20, true), "", false},
		{"beyond buffer cap", proxyTestBody("test-model", proxyMaxBodyBuffer, true), "body_too_large", false},
		{"not json", []byte("not-json"), "body_not_json", false},
		{"missing model", []byte(`{"messages":[]}`), "missing_model", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(tc.body))
			plan, pb, err := resolveProxyHostByPolicy(r, resolver, rk, false)
			if tc.wantReason != "" {
				e, ok := err.(*errProxyHostResolve)
				if !ok || e.Reason != tc.wantReason {
					t.Fatalf("err: got %v, want reason %q", err, tc.wantReason)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if plan == nil || plan.Host == nil || plan.Host.Meta.Name != "openai" {
				t.Fatalf("plan host: %+v", plan)
			}
			if pb.Truncated != tc.wantTruncated {
				t.Fatalf("truncated: got %v, want %v", pb.Truncated, tc.wantTruncated)
			}
			got, rerr := io.ReadAll(pb.Reader)
			if rerr != nil {
				t.Fatalf("read forwarded body: %v", rerr)
			}
			if !bytes.Equal(got, tc.body) {
				t.Fatalf("forwarded bytes differ: got %d, want %d", len(got), len(tc.body))
			}
			if tc.wantTruncated {
				if len(pb.Prefix) != proxyMaxBodyPeek+1 || !bytes.Equal(pb.Prefix, tc.body[:len(pb.Prefix)]) {
					t.Fatalf("prefix: len %d, want peek-window prefix of the body", len(pb.Prefix))
				}
			} else if !bytes.Equal(pb.Prefix, tc.body) {
				t.Fatalf("prefix should be the full body, got %d bytes", len(pb.Prefix))
			}
		})
	}
}

// TestProxyForward_StreamedBodyByteIdentical drives the streamed (>peek)
// resolve result through the real proxy forwarder against an httptest
// upstream, verifying the upstream receives the byte-identical body with
// the original Content-Length (no chunked degradation).
func TestProxyForward_StreamedBodyByteIdentical(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	resolver := routing.New(cat)
	body := proxyTestBody("test-model", 2<<20, false)

	var gotBody []byte
	var gotCL int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotCL = r.ContentLength
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	_, pb, err := resolveProxyHostByPolicy(r, resolver, rk, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !pb.Truncated {
		t.Fatal("expected the streamed (truncated-prefix) path")
	}

	p := proxy.New(nil, nil, nil)
	res, err := p.Run(context.Background(), &proxy.Request{
		Method:        http.MethodPost,
		Path:          "/v1/chat/completions",
		Body:          pb.Reader,
		ContentLength: r.ContentLength,
		Headers:       http.Header{},
		HostBaseURL:   srv.URL,
		UpstreamAuth:  "Bearer upstream-key",
	})
	if err != nil {
		t.Fatalf("proxy run: %v", err)
	}
	_, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()

	if !bytes.Equal(gotBody, body) {
		t.Fatalf("upstream bytes differ: got %d, want %d", len(gotBody), len(body))
	}
	if gotCL != int64(len(body)) {
		t.Fatalf("upstream Content-Length: got %d, want %d", gotCL, len(body))
	}
}

// TestProxyForward_PayloadTeeCapture drives the streamed (>peek) resolve
// result through the real forwarder with the payload-capture tee armed,
// asserting the upstream bytes stay identical while the lifecycle context
// ends up with the full body — or, past the capture cap, a flagged
// partial one with the upstream forward unaffected.
func TestProxyForward_PayloadTeeCapture(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	resolver := routing.New(cat)

	cases := []struct {
		name       string
		padding    int
		logging    bool
		wantStored int // expected lc.RequestBody length; -1 = whole body
		wantTrunc  bool
	}{
		{"logging on captures full body", 3 << 20, true, -1, false},
		{"logging off keeps prefix only", 3 << 20, false, proxyMaxBodyPeek + 1, true},
		{"capture cap exceeded", proxyMaxBodyBuffer, true, proxyMaxBodyBuffer, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := proxyTestBody("test-model", tc.padding, false)
			var gotBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			lc := lifecycle.NewContext("req-tee", "proxy", time.Now())
			lc.PayloadLog = tc.logging

			_, pb, err := resolveProxyHostByPolicy(r, resolver, rk, lc.PayloadLog)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if armed := pb.Capture != nil; armed != tc.logging {
				t.Fatalf("capture armed: got %v, want %v", armed, tc.logging)
			}
			pb.seedLifecycle(lc)

			p := proxy.New(nil, nil, nil)
			res, err := p.Run(context.Background(), &proxy.Request{
				Method:        http.MethodPost,
				Path:          "/v1/chat/completions",
				Body:          pb.Reader,
				ContentLength: r.ContentLength,
				Headers:       http.Header{},
				HostBaseURL:   srv.URL,
				UpstreamAuth:  "Bearer upstream-key",
				Lifecycle:     lc,
			})
			if err != nil {
				t.Fatalf("proxy run: %v", err)
			}
			rb := pb.wrapResult(res.Body, lc)
			_, _ = io.ReadAll(rb)
			_ = rb.Close()

			if !bytes.Equal(gotBody, body) {
				t.Fatalf("upstream bytes differ: got %d, want %d", len(gotBody), len(body))
			}
			want := body
			if tc.wantStored >= 0 {
				want = body[:tc.wantStored]
			}
			if !bytes.Equal(lc.RequestBody, want) {
				t.Fatalf("stored body: got %d bytes, want %d", len(lc.RequestBody), len(want))
			}
			if lc.RequestBodyTruncated != tc.wantTrunc {
				t.Fatalf("truncated: got %v, want %v", lc.RequestBodyTruncated, tc.wantTrunc)
			}
		})
	}
}

// TestProxyForward_TeeStreamsWhileClientSending pins the latency
// invariant: with the capture tee armed, body bytes past the peek window
// reach the upstream while the client still withholds the tail (a
// buffer-first regression deadlocks and trips the watchdog). The
// post-flight hook then reads lc.RequestBody from its own goroutine —
// under -race this exercises the concurrent send + post-flight handoff.
func TestProxyForward_TeeStreamsWhileClientSending(t *testing.T) {
	cat, rk := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	resolver := routing.New(cat)
	body := proxyTestBody("test-model", 2<<20, false)
	cut := len(body) - (256 << 10) // tail the client withholds
	// Receiving past this proves upstream consumed live-streamed bytes
	// beyond the buffered peek prefix.
	threshold := proxyMaxBodyPeek + (64 << 10)

	upstreamSawStream := make(chan struct{})
	upstreamBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got bytes.Buffer
		buf := make([]byte, 32<<10)
		signaled := false
		for {
			n, err := r.Body.Read(buf)
			got.Write(buf[:n])
			if !signaled && got.Len() >= threshold {
				signaled = true
				close(upstreamSawStream)
			}
			if err != nil {
				break
			}
		}
		upstreamBody <- got.Bytes()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write(body[:cut])
		<-upstreamSawStream // hold the tail until upstream proves receipt
		_, _ = pw.Write(body[cut:])
		_ = pw.Close()
	}()

	type seen struct {
		body  []byte
		trunc bool
	}
	hookSaw := make(chan seen, 1)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{
		HookName: "test-capture",
		Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
			hookSaw <- seen{body: lc.RequestBody, trunc: lc.RequestBodyTruncated}
			return nil, nil
		},
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", pr)
		lc := lifecycle.NewContext("req-stream", "proxy", time.Now())
		lc.PayloadLog = true
		_, pb, err := resolveProxyHostByPolicy(r, resolver, rk, true)
		if err != nil {
			t.Errorf("resolve: %v", err)
			return
		}
		pb.seedLifecycle(lc)
		p := proxy.New(nil, reg, nil)
		res, err := p.Run(context.Background(), &proxy.Request{
			Method:       http.MethodPost,
			Path:         "/v1/chat/completions",
			Body:         pb.Reader,
			Headers:      http.Header{},
			HostBaseURL:  srv.URL,
			UpstreamAuth: "Bearer upstream-key",
			Lifecycle:    lc,
		})
		if err != nil {
			t.Errorf("proxy run: %v", err)
			return
		}
		rb := pb.wrapResult(res.Body, lc)
		_, _ = io.ReadAll(rb)
		_ = rb.Close()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("watchdog: upstream never received streamed bytes while the client was still sending (buffer-first regression?)")
	}
	if got := <-upstreamBody; !bytes.Equal(got, body) {
		t.Fatalf("upstream bytes differ: got %d, want %d", len(got), len(body))
	}
	select {
	case saw := <-hookSaw:
		if !bytes.Equal(saw.body, body) {
			t.Fatalf("post-flight capture: got %d bytes, want the full %d", len(saw.body), len(body))
		}
		if saw.trunc {
			t.Fatal("complete capture must not be flagged truncated")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight hook never fired")
	}
}

// TestBodyCapture_ConcurrentFinalize races finalizeInto against a
// transport-like reader still draining the tee: the published snapshot
// must always be a consistent prefix of the body, stable after finalize,
// and flagged truncated whenever it is partial. Meaningful under -race.
func TestBodyCapture_ConcurrentFinalize(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
	prefix := append([]byte(nil), body[:1024]...)
	c := newBodyCapture(prefix, len(body))
	tee := c.tee(bytes.NewReader(body[1024:]))

	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, tee)
		close(done)
	}()

	lc := lifecycle.NewContext("req-race", "proxy", time.Now())
	c.finalizeInto(lc)
	if !bytes.Equal(lc.RequestBody, body[:len(lc.RequestBody)]) {
		t.Fatal("published capture is not a prefix of the body")
	}
	if len(lc.RequestBody) < len(body) && !lc.RequestBodyTruncated {
		t.Fatal("partial capture must be flagged truncated")
	}
	snap := append([]byte(nil), lc.RequestBody...)
	<-done
	if !bytes.Equal(lc.RequestBody, snap) {
		t.Fatal("published capture mutated after finalize")
	}
}
