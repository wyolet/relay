package adapter_test

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/pkg/adapters"
	pkgusage "github.com/wyolet/relay/sdk/usage"
)

// newBearerSpec returns a minimal built Spec for the openai name.
func newBearerSpec(path string, opts ...func(*adapter.Spec)) *adapter.Spec {
	s := &adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: path,
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
	}
	for _, o := range opts {
		o(s)
	}
	return s.Build()
}

// dualAuthSpec has an API-key auth and an OAuth variant, each with its own static headers.
func dualAuthSpec() *adapter.Spec {
	return (&adapter.Spec{
		Name:        adapters.Anthropic,
		DefaultPath: "/v1/messages",
		Auth: adapter.AuthStrategy{
			Header:       "x-api-key",
			ExtraHeaders: map[string]string{"x-api-version": "v1"},
		},
		OAuthAuth: adapter.AuthStrategy{
			Header:       "Authorization",
			Scheme:       "Bearer",
			ExtraHeaders: map[string]string{"x-api-version": "v1", "x-oauth-beta": "beta-v1"},
		},
	}).Build()
}

func pathFnSpec() *adapter.Spec {
	return newBearerSpec("/ignored", func(s *adapter.Spec) {
		s.UpstreamPathFn = func(model string, stream bool) string {
			if stream {
				return "/models/" + model + ":stream?alt=sse"
			}
			return "/models/" + model + ":generate"
		}
	})
}

func ptr(s string) *string { return &s }

// Every case pins the full request the upstream receives: URI and the complete header set.
func TestSpecAdapter_Call(t *testing.T) {
	tests := []struct {
		name     string
		spec     *adapter.Spec
		hostPath *string
		key      string
		hdr      http.Header
		model    string
		stream   bool
		oauth    bool
		wantURI  string
		// wantHdr is added to the headers the Go client always sends.
		wantHdr http.Header
	}{
		{
			name: "credential with scheme on the default path",
			spec: newBearerSpec("/v1/chat/completions"), key: "sk-test",
			wantURI: "/v1/chat/completions",
			wantHdr: http.Header{"Authorization": {"Bearer sk-test"}},
		},
		{
			name:    "empty credential sets no auth header",
			spec:    newBearerSpec("/v1/chat/completions"),
			wantURI: "/v1/chat/completions",
		},
		{
			name: "forwarded headers arrive with every value",
			spec: newBearerSpec("/v1/chat/completions"), key: "k",
			hdr:     http.Header{"Accept": {"application/json"}, "User-Agent": {"client/1.0"}, "X-Multi": {"a", "b"}},
			wantURI: "/v1/chat/completions",
			wantHdr: http.Header{"Authorization": {"Bearer k"}, "Accept": {"application/json"}, "User-Agent": {"client/1.0"}, "X-Multi": {"a", "b"}},
		},
		{
			name: "relay's credential and content type replace forwarded ones",
			spec: newBearerSpec("/v1/chat/completions"), key: "upstream",
			hdr:     http.Header{"Authorization": {"Bearer caller"}, "Content-Type": {"text/plain"}},
			wantURI: "/v1/chat/completions",
			wantHdr: http.Header{"Authorization": {"Bearer upstream"}},
		},
		{
			name: "api key without scheme plus the static header",
			spec: dualAuthSpec(), key: "sk-key",
			wantURI: "/v1/messages",
			wantHdr: http.Header{"X-Api-Key": {"sk-key"}, "X-Api-Version": {"v1"}},
		},
		{
			name: "forwarded header beats the static default",
			spec: dualAuthSpec(), key: "sk-key",
			hdr:     http.Header{"X-Api-Version": {"v2"}},
			wantURI: "/v1/messages",
			wantHdr: http.Header{"X-Api-Key": {"sk-key"}, "X-Api-Version": {"v2"}},
		},
		{
			name: "oauth credential uses the OAuth variant only",
			spec: dualAuthSpec(), key: "tok", oauth: true,
			wantURI: "/v1/messages",
			wantHdr: http.Header{"Authorization": {"Bearer tok"}, "X-Api-Version": {"v1"}, "X-Oauth-Beta": {"beta-v1"}},
		},
		{
			name: "oauth credential overrides a forwarded Authorization",
			spec: dualAuthSpec(), key: "tok", oauth: true,
			hdr:     http.Header{"Authorization": {"Bearer caller"}},
			wantURI: "/v1/messages",
			wantHdr: http.Header{"Authorization": {"Bearer tok"}, "X-Api-Version": {"v1"}, "X-Oauth-Beta": {"beta-v1"}},
		},
		{
			name: "oauth credential on a spec without a variant uses the default auth",
			spec: newBearerSpec("/v1/chat/completions"), key: "sk-tok", oauth: true,
			wantURI: "/v1/chat/completions",
			wantHdr: http.Header{"Authorization": {"Bearer sk-tok"}},
		},
		{
			name: "host path wins verbatim",
			spec: newBearerSpec("/v1/responses"), hostPath: ptr("/backend-api/codex/responses"),
			wantURI: "/backend-api/codex/responses",
		},
		{
			name: "explicit empty host path appends nothing",
			spec: newBearerSpec("/v1/responses"), hostPath: ptr(""),
			wantURI: "/",
		},
		{
			name: "path builder, buffered",
			spec: pathFnSpec(), model: "m-1.5",
			wantURI: "/models/m-1.5:generate",
		},
		{
			name: "path builder, streamed",
			spec: pathFnSpec(), model: "m-1.5", stream: true,
			wantURI: "/models/m-1.5:stream?alt=sse",
		},
		{
			name: "path builder gets the model as one escaped segment",
			spec: pathFnSpec(), model: "a/../b?x=1",
			wantURI: "/models/a%2F..%2Fb%3Fx=1:generate",
		},
		{
			name: "host path beats the path builder",
			spec: pathFnSpec(), model: "m", hostPath: ptr("/fixed"),
			wantURI: "/fixed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseURL, last := captureServer(t)
			before := tt.hdr.Clone()
			resp, err := tt.spec.PipelineAdapter().Call(context.Background(), baseURL, tt.hostPath, tt.key, []byte(`{}`), tt.hdr, tt.model, tt.stream, tt.oauth)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			resp.Body.Close()

			want := http.Header{
				"Accept-Encoding": {"gzip"},
				"Content-Length":  {"2"},
				"Content-Type":    {"application/json"},
				"User-Agent":      {"Go-http-client/1.1"},
			}
			for k, v := range tt.wantHdr {
				want[k] = v
			}
			if diff := cmp.Diff(captured{Method: http.MethodPost, URI: tt.wantURI, Header: want, Body: []byte(`{}`)}, last()); diff != "" {
				t.Errorf("upstream request (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(before, tt.hdr); diff != "" {
				t.Errorf("Call changed the caller's headers (-before +after):\n%s", diff)
			}
		})
	}
}

// The body reaches the upstream byte for byte on both sides of the size where the body reader changes.
func TestSpecAdapter_Call_BodySizes(t *testing.T) {
	baseURL, last := captureServer(t)
	a := newBearerSpec("/v1/chat/completions").PipelineAdapter()
	for _, size := range []int{0, 3 << 10, 64 << 10, 64<<10 + 1, 1 << 20} {
		body := bytes.Repeat([]byte("x"), size)
		resp, err := a.Call(context.Background(), baseURL, nil, "key", body, nil, "", false, false)
		if err != nil {
			t.Fatalf("size %d: Call: %v", size, err)
		}
		resp.Body.Close()
		got := last()
		if !bytes.Equal(got.Body, body) {
			t.Errorf("size %d: upstream read %d bytes", size, len(got.Body))
		}
		if cl := got.Header.Get("Content-Length"); cl != strconv.Itoa(size) {
			t.Errorf("size %d: Content-Length = %q", size, cl)
		}
	}
}

func TestSpecAdapter_Call_Errors(t *testing.T) {
	a := newBearerSpec("/v1/chat/completions").PipelineAdapter()
	for name, baseURL := range map[string]string{
		"unparseable base URL": "http://bad host",
		"nothing listening":    "http://127.0.0.1:1",
	} {
		t.Run(name, func(t *testing.T) {
			resp, err := a.Call(context.Background(), baseURL, nil, "k", []byte(`{}`), nil, "", false, false)
			if err == nil || resp != nil {
				t.Fatalf("resp = %v, err = %v; want a nil response and an error", resp, err)
			}
		})
	}
}

func TestSpecAdapter_Retryable(t *testing.T) {
	a := newBearerSpec("/v1/chat/completions").PipelineAdapter()
	resp := func(status int, retryAfter string) *http.Response {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return &http.Response{StatusCode: status, Header: h}
	}
	tests := []struct {
		name      string
		resp      *http.Response
		wantRetry bool
		wantKind  keypool.FailureKind
		wantAfter time.Duration
	}{
		{"nil response", nil, false, 0, 0},
		{"200", resp(200, ""), false, 0, 0},
		{"400", resp(400, ""), false, 0, 0},
		{"404", resp(404, ""), false, 0, 0},
		{"499 is below the server range", resp(499, ""), false, 0, 0},
		{"600 is above the server range", resp(600, ""), false, 0, 0},
		{"401", resp(401, ""), true, keypool.FailureAuth, 0},
		{"403", resp(403, ""), true, keypool.FailureForbidden, 0},
		{"500", resp(500, ""), true, keypool.FailureServerError, 0},
		{"529", resp(529, ""), true, keypool.FailureServerError, 0},
		{"599", resp(599, ""), true, keypool.FailureServerError, 0},
		{"500 ignores Retry-After", resp(500, "30"), true, keypool.FailureServerError, 0},
		{"429 without Retry-After", resp(429, ""), true, keypool.FailureRateLimitShort, 0},
		{"429 at the 5s boundary stays short", resp(429, "5"), true, keypool.FailureRateLimitShort, 5 * time.Second},
		{"429 past 5s is long", resp(429, "6"), true, keypool.FailureRateLimitLong, 6 * time.Second},
		{"429 with an unreadable Retry-After", resp(429, "soon"), true, keypool.FailureRateLimitShort, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retry, kind, after := a.Retryable(tt.resp)
			if retry != tt.wantRetry || kind != tt.wantKind || after != tt.wantAfter {
				t.Errorf("Retryable = (%v, %v, %v), want (%v, %v, %v)", retry, kind, after, tt.wantRetry, tt.wantKind, tt.wantAfter)
			}
		})
	}
}

func TestSpecAdapter_ExtractTokens(t *testing.T) {
	if got := newBearerSpec("/x").PipelineAdapter().ExtractTokens([]byte(`{"id":"x"}`)); got != nil {
		t.Errorf("no extractor: tokens = %v, want nil", got)
	}
	var seen []byte
	a := newBearerSpec("/x", func(s *adapter.Spec) {
		s.ExtractTokens = func(body []byte) pkgusage.Tokens {
			seen = body
			return pkgusage.Tokens{"input": 10, "output": 5}
		}
	}).PipelineAdapter()
	got := a.ExtractTokens([]byte(`{"u":1}`))
	if diff := cmp.Diff(pkgusage.Tokens{"input": 10, "output": 5}, got); diff != "" {
		t.Errorf("tokens (-want +got):\n%s", diff)
	}
	if string(seen) != `{"u":1}` {
		t.Errorf("extractor saw %q, want the response body", seen)
	}
}
