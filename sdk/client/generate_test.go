package client

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/sdk/adapters/anthropic"
	"github.com/wyolet/relay/sdk/adapters/gemini"
	"github.com/wyolet/relay/sdk/adapters/openai"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Every constructor sends one POST whose path, auth header and body are fixed by its wire shape; the body is the shape's own serialization of the request with the output mode set.
func TestGenerate_SendsTheShapesRequest(t *testing.T) {
	for _, k := range []string{EnvUsage, EnvHeaders, EnvTimeout} {
		t.Setenv(k, "")
	}
	tests := []struct {
		name       string
		build      func(url string) *Client
		translator v1.Translator
		stream     bool
		path       string
		query      string
		header     http.Header // exact values of every header the client sets
	}{
		{name: "relay", build: func(u string) *Client { return Relay(u, "rk") }, translator: v1.IdentityTranslator{},
			path: "/v1/generate", header: http.Header{"Authorization": {"Bearer rk"}}},
		{name: "relay stream", build: func(u string) *Client { return Relay(u, "rk") }, translator: v1.IdentityTranslator{}, stream: true,
			path: "/v1/generate", header: http.Header{"Authorization": {"Bearer rk"}}},
		{name: "chat completions", build: func(u string) *Client { return OpenAI(u, "sk") }, translator: openai.CCTranslator{},
			path: "/v1/chat/completions", header: http.Header{"Authorization": {"Bearer sk"}}},
		{name: "responses", build: func(u string) *Client { return OpenAIResponses(u, "sk") }, translator: openai.ResponsesTranslator{},
			path: "/v1/responses", header: http.Header{"Authorization": {"Bearer sk"}}},
		{name: "messages", build: func(u string) *Client { return Anthropic(u, "ak") }, translator: anthropic.AnthropicTranslator{},
			path: "/v1/messages", header: http.Header{"X-Api-Key": {"ak"}, "Anthropic-Version": {"2023-06-01"}}},
		{name: "generateContent", build: func(u string) *Client { return Gemini(u, "gk") }, translator: gemini.GeminiTranslator{},
			path: "/v1beta/models/some-model:generateContent", header: http.Header{"X-Goog-Api-Key": {"gk"}}},
		{name: "streamGenerateContent", build: func(u string) *Client { return Gemini(u, "gk") }, translator: gemini.GeminiTranslator{}, stream: true,
			path: "/v1beta/models/some-model:streamGenerateContent", query: "alt=sse", header: http.Header{"X-Goog-Api-Key": {"gk"}}},
		{name: "trailing slashes on the base URL are dropped", build: func(u string) *Client { return OpenAI(u+"//", "sk") }, translator: openai.CCTranslator{},
			path: "/v1/chat/completions", header: http.Header{"Authorization": {"Bearer sk"}}},
		{name: "options override path, auth and headers", translator: openai.CCTranslator{},
			build: func(u string) *Client {
				return OpenAI(u, "sk", WithPath("/custom"), WithAuth(Auth{Header: "api-key"}), WithHeader("X-One", "1"), WithHeader("X-Two", "2"))
			},
			path: "/custom", header: http.Header{"Api-Key": {"sk"}, "X-One": {"1"}, "X-Two": {"2"}}},
		{name: "path function option", translator: openai.CCTranslator{},
			build: func(u string) *Client {
				return OpenAI(u, "sk", WithPathFn(func(model string, stream bool) string { return "/deployments/" + model }))
			},
			path: "/deployments/some-model", header: http.Header{"Authorization": {"Bearer sk"}}},
		{name: "New with a bare translator", translator: v1.IdentityTranslator{},
			build: func(u string) *Client { return New(v1.IdentityTranslator{}, u+"/", "/p", "k") },
			path:  "/p", header: http.Header{"Authorization": {"Bearer k"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// The 204 leaves nothing to parse: only what was sent matters here.
			rec := newRecorder(t, http.StatusNoContent, "")
			c := tc.build(rec.srv.URL)
			req, mode := sampleReq(), v1.OutputModeSync
			if tc.stream {
				mode = v1.OutputModeStream
				s, err := c.GenerateStream(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				_ = s.Close()
			} else {
				_, _ = c.Generate(context.Background(), req)
			}
			if diff := cmp.Diff(sampleReq(), req); diff != "" {
				t.Errorf("the caller's request was changed (-before +after):\n%s", diff)
			}

			got := rec.only(t)
			withMode := sampleReq()
			withMode.OutputMode = mode
			wantBody, err := tc.translator.SerializeRequest(withMode)
			if err != nil {
				t.Fatal(err)
			}
			if got.Method != http.MethodPost || got.Path != tc.path || got.Query != tc.query {
				t.Errorf("sent %s %s?%s, want POST %s?%s", got.Method, got.Path, got.Query, tc.path, tc.query)
			}
			if got.Body != string(wantBody) {
				t.Errorf("body:\n got %s\nwant %s", got.Body, wantBody)
			}
			for _, h := range []string{"Accept-Encoding", "Content-Length", "User-Agent"} {
				got.Header.Del(h)
			}
			want := tc.header.Clone()
			want.Set("Content-Type", "application/json")
			if diff := cmp.Diff(want, got.Header); diff != "" {
				t.Errorf("headers (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGenerate_RelayBodyAndResponse(t *testing.T) {
	rec := newRecorder(t, http.StatusOK, canonicalReply)
	resp, err := Relay(rec.srv.URL, "rk").Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.only(t).Body; got != sampleWire("sync") {
		t.Errorf("body:\n got %s\nwant %s", got, sampleWire("sync"))
	}
	want, err := v1.ParseResponse([]byte(canonicalReply))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, resp.Response); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
	if cost, ok := resp.Cost(); ok || cost != 0 {
		t.Errorf("Cost = %v, %v; a relay target has no SDK-side price", cost, ok)
	}
}

func TestGenerate_VendorResponseBecomesCanonical(t *testing.T) {
	rec := newRecorder(t, http.StatusOK, ccReply)
	resp, err := OpenAI(rec.srv.URL, "sk").Generate(context.Background(), sampleReq())
	if err != nil {
		t.Fatal(err)
	}
	want, err := openai.CCTranslator{}.ParseResponse([]byte(ccReply))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, resp.Response); diff != "" {
		t.Errorf("response (-want +got):\n%s", diff)
	}
	if resp.FinishReason != v1.FinishReasonStop || resp.Usage["input"] != 1000 || resp.Usage["output"] != 500 {
		t.Errorf("finish %q usage %v, want stop and 1000/500", resp.FinishReason, resp.Usage)
	}
}

// A non-2xx answer is an *APIError carrying the host, status and the vendor's own code and message, on both call kinds.
func TestGenerate_APIError(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		code    string
		message string
		text    string // Error() after the host
	}{
		{"error.code", 400, `{"error":{"code":"translate_request","message":"input is required"}}`, "translate_request", "input is required", ": 400 translate_request: input is required"},
		{"error.type when there is no code", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`, "authentication_error", "invalid x-api-key", ": 401 authentication_error: invalid x-api-key"},
		{"code wins over type", 429, `{"error":{"code":"rate_limit","type":"requests","message":"slow down"}}`, "rate_limit", "slow down", ": 429 rate_limit: slow down"},
		{"body that is not JSON", 502, `<html>Bad Gateway</html>`, "", "", ": 502: <html>Bad Gateway</html>"},
		{"JSON without an error object", 500, `{"detail":"boom"}`, "", "", `: 500: {"detail":"boom"}`},
		{"empty body", 503, ``, "", "", ": 503: "},
		{"redirect is not followed into success", 304, ``, "", "", ": 304: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := newRecorder(t, tc.status, tc.body)
			want := &APIError{StatusCode: tc.status, Host: rec.host(), Code: tc.code, Message: tc.message, Raw: []byte(tc.body)}
			if tc.body == "" {
				want.Raw = []byte{}
			}
			c := Relay(rec.srv.URL, "rk")

			resp, err := c.Generate(context.Background(), sampleReq())
			var apiErr *APIError
			if resp != nil || !errors.As(err, &apiErr) {
				t.Fatalf("Generate = %v, %v; want an *APIError", resp, err)
			}
			if diff := cmp.Diff(want, apiErr); diff != "" {
				t.Errorf("Generate error (-want +got):\n%s", diff)
			}
			if got := apiErr.Error(); got != rec.host()+tc.text {
				t.Errorf("Error() = %q, want %q", got, rec.host()+tc.text)
			}

			stream, err := c.GenerateStream(context.Background(), sampleReq())
			apiErr = nil
			if stream != nil || !errors.As(err, &apiErr) {
				t.Fatalf("GenerateStream = %v, %v; want an *APIError", stream, err)
			}
			if diff := cmp.Diff(want, apiErr); diff != "" {
				t.Errorf("GenerateStream error (-want +got):\n%s", diff)
			}
		})
	}
}

func TestGenerate_Failures(t *testing.T) {
	t.Run("2xx body the shape cannot parse", func(t *testing.T) {
		rec := newRecorder(t, http.StatusOK, `not json`)
		resp, err := Relay(rec.srv.URL, "rk").Generate(context.Background(), sampleReq())
		var apiErr *APIError
		if resp != nil || err == nil || errors.As(err, &apiErr) {
			t.Fatalf("Generate = %v, %v; want the parse error", resp, err)
		}
	})
	t.Run("unreachable host", func(t *testing.T) {
		c := Relay("http://127.0.0.1:1", "rk")
		if resp, err := c.Generate(context.Background(), sampleReq()); resp != nil || err == nil {
			t.Errorf("Generate = %v, %v; want a dial error", resp, err)
		}
		if s, err := c.GenerateStream(context.Background(), sampleReq()); s != nil || err == nil {
			t.Errorf("GenerateStream = %v, %v; want a dial error", s, err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		rec := newRecorder(t, http.StatusOK, canonicalReply)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Relay(rec.srv.URL, "rk").Generate(ctx, sampleReq()); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("request the shape cannot serialize", func(t *testing.T) {
		rec := newRecorder(t, http.StatusOK, canonicalReply)
		c := New(unserializable{}, rec.srv.URL, "/p", "k")
		if _, err := c.Generate(context.Background(), sampleReq()); err == nil || err.Error() != "relay client: serialize request: no such shape" {
			t.Errorf("Generate err = %v", err)
		}
		if _, err := c.GenerateStream(context.Background(), sampleReq()); err == nil || err.Error() != "relay client: serialize request: no such shape" {
			t.Errorf("GenerateStream err = %v", err)
		}
		if got := len(rec.reqs); got != 0 {
			t.Errorf("%d requests sent for a request that failed to serialize", got)
		}
	})
}

// unserializable is a shape that rejects every request.
type unserializable struct{ v1.IdentityTranslator }

func (unserializable) SerializeRequest(*v1.Request) ([]byte, error) {
	return nil, errors.New("no such shape")
}

func TestClient_Host(t *testing.T) {
	for base, want := range map[string]string{
		"https://api.example.com/v1/": "api.example.com",
		"http://127.0.0.1:8080":       "127.0.0.1:8080",
		"api.example.com":             "api.example.com",
		"":                            "",
	} {
		if got := New(v1.IdentityTranslator{}, base, "/p", "k").host(); got != want {
			t.Errorf("host of %q = %q, want %q", base, got, want)
		}
	}
}
