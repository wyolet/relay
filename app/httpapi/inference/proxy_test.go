package inference

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	apphost "github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/routing"
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
			plan, pb, err := resolveProxyHostByPolicy(r, resolver, rk)
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
	_, pb, err := resolveProxyHostByPolicy(r, resolver, rk)
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
