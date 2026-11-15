package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// Relay and RelayWS resolve the same configuration: explicit arguments, then WR_* variables; a bad or missing value is held back until the first call.
func TestRelay_Config(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		base    string
		key     string
		opts    []Option
		wantURL string
		wantKey string
		headers map[string]string
		timeout time.Duration
		err     string
	}{
		{name: "explicit arguments", base: "http://explicit.example/", key: "rk-explicit", wantURL: "http://explicit.example", wantKey: "rk-explicit"},
		{name: "explicit arguments beat the environment", env: map[string]string{EnvBaseURL: "http://env.example", EnvAPIKey: "rk-env"},
			base: "http://explicit.example", key: "rk-explicit", wantURL: "http://explicit.example", wantKey: "rk-explicit"},
		{name: "environment fills what is not passed",
			env:     map[string]string{EnvBaseURL: "http://env.example:8080", EnvAPIKey: "rk-env", EnvUsage: "full", EnvHeaders: "X-Custom=hi, X-Other = 2 ", EnvTimeout: "42s"},
			wantURL: "http://env.example:8080", wantKey: "rk-env", timeout: 42 * time.Second,
			headers: map[string]string{"X-WR-Usage": "full", "X-Custom": "hi", "X-Other": "2"}},
		{name: "an explicit header beats WR_USAGE and WR_HEADERS", env: map[string]string{EnvUsage: "full", EnvHeaders: "X-Custom=env"},
			base: "http://x.example", key: "rk", opts: []Option{WithHeader("X-WR-Usage", "off"), WithHeader("X-Custom", "explicit")},
			wantURL: "http://x.example", wantKey: "rk", headers: map[string]string{"X-WR-Usage": "off", "X-Custom": "explicit"}},
		{name: "nothing configured", err: "relay client: missing config — set WR_BASE_URL and WR_API_KEY or pass explicitly"},
		{name: "key missing", base: "http://x.example", wantURL: "http://x.example", err: "relay client: missing config — set WR_API_KEY or pass explicitly"},
		{name: "base URL missing", key: "rk", wantKey: "rk", err: "relay client: missing config — set WR_BASE_URL or pass explicitly"},
		{name: "malformed WR_HEADERS", env: map[string]string{EnvHeaders: "X-Ok=1,no-equals-sign"}, base: "http://x.example", key: "rk",
			wantURL: "http://x.example", wantKey: "rk", err: `relay client: invalid WR_HEADERS entry "no-equals-sign" (want k=v)`},
		{name: "malformed WR_TIMEOUT", env: map[string]string{EnvTimeout: "soon"}, base: "http://x.example", key: "rk",
			wantURL: "http://x.example", wantKey: "rk", err: `relay client: invalid WR_TIMEOUT "soon": time: invalid duration "soon"`},
		{name: "every problem is reported at once", env: map[string]string{EnvHeaders: "bad", EnvTimeout: "soon"},
			err: "relay client: missing config — set WR_BASE_URL and WR_API_KEY or pass explicitly\n" +
				`relay client: invalid WR_HEADERS entry "bad" (want k=v)` + "\n" +
				`relay client: invalid WR_TIMEOUT "soon": time: invalid duration "soon"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{EnvBaseURL, EnvAPIKey, EnvUsage, EnvHeaders, EnvTimeout} {
				t.Setenv(k, tc.env[k])
			}
			for kind, c := range map[string]*Client{"Relay": Relay(tc.base, tc.key, tc.opts...), "RelayWS": RelayWS(tc.base, tc.key, tc.opts...)} {
				if c.baseURL != tc.wantURL || c.apiKey != tc.wantKey || c.syncTimeout != tc.timeout {
					t.Errorf("%s: base %q key %q timeout %v; want %q %q %v", kind, c.baseURL, c.apiKey, c.syncTimeout, tc.wantURL, tc.wantKey, tc.timeout)
				}
				if diff := cmp.Diff(tc.headers, c.headers); diff != "" {
					t.Errorf("%s: headers (-want +got):\n%s", kind, diff)
				}
				got := ""
				if c.configErr != nil {
					got = c.configErr.Error()
				}
				if got != tc.err {
					t.Errorf("%s: config error %q, want %q", kind, got, tc.err)
				}
				if tc.err == "" {
					continue
				}
				// The held error comes back from every call, with nothing sent.
				ctx := context.Background()
				_, genErr := c.Generate(ctx, sampleReq())
				_, streamErr := c.GenerateStream(ctx, sampleReq())
				_, modelsErr := c.Models(ctx)
				_, catalogErr := c.Catalog(ctx)
				for call, err := range map[string]error{"Generate": genErr, "GenerateStream": streamErr, "Models": modelsErr, "Catalog": catalogErr} {
					if !errors.Is(err, c.configErr) {
						t.Errorf("%s: %s err = %v, want the config error", kind, call, err)
					}
				}
			}
		})
	}
}

func TestRelay_PathsAndTransports(t *testing.T) {
	r, ws := Relay("http://x.example", "rk"), RelayWS("http://x.example", "rk")
	if _, ok := r.transport.(httpTransport); !ok || r.path != "/v1/generate" || r.relay == nil {
		t.Errorf("Relay: transport %T path %q relay %v", r.transport, r.path, r.relay)
	}
	if _, ok := ws.transport.(*wsTransport); !ok || ws.path != "/v1/ws" || ws.relay == nil {
		t.Errorf("RelayWS: transport %T path %q relay %v", ws.transport, ws.path, ws.relay)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close on an HTTP client = %v", err)
	}
	if err := ws.Close(); err != nil {
		t.Errorf("Close on a never-dialed WS client = %v", err)
	}
}

// WR_TIMEOUT bounds a sync call and leaves a stream alone.
func TestRelay_TimeoutAppliesToSyncOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(canonicalStream))
	}))
	defer srv.Close()
	for _, k := range []string{EnvUsage, EnvHeaders} {
		t.Setenv(k, "")
	}
	t.Setenv(EnvTimeout, "20ms")
	c := Relay(srv.URL, "rk")

	if _, err := c.Generate(context.Background(), sampleReq()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Generate err = %v, want context.DeadlineExceeded", err)
	}
	if got := len(drain(t, c)); got != 3 {
		t.Errorf("stream delivered %d events past the sync timeout, want all 3", got)
	}
}

// A vendor constructor reads only its own conventional key variables and never the relay's.
func TestVendorClients_KeyFromEnvironment(t *testing.T) {
	tests := []struct {
		name  string
		build func(key string) *Client
		env   map[string]string
		key   string
		want  string
	}{
		{"OpenAI from OPENAI_API_KEY", func(k string) *Client { return OpenAI("https://x.example", k) }, map[string]string{EnvOpenAIKey: "sk-env"}, "", "sk-env"},
		{"OpenAI explicit wins", func(k string) *Client { return OpenAI("https://x.example", k) }, map[string]string{EnvOpenAIKey: "sk-env"}, "sk-explicit", "sk-explicit"},
		{"OpenAIResponses from OPENAI_API_KEY", func(k string) *Client { return OpenAIResponses("https://x.example", k) }, map[string]string{EnvOpenAIKey: "sk-env"}, "", "sk-env"},
		{"Anthropic from ANTHROPIC_API_KEY", func(k string) *Client { return Anthropic("https://x.example", k) }, map[string]string{EnvAnthropicKey: "ak-env", EnvOpenAIKey: "sk-env"}, "", "ak-env"},
		{"Gemini prefers GEMINI_API_KEY", func(k string) *Client { return Gemini("https://x.example", k) }, map[string]string{EnvGeminiKey: "gemini-env", EnvGoogleKey: "google-env"}, "", "gemini-env"},
		{"Gemini falls back to GOOGLE_API_KEY", func(k string) *Client { return Gemini("https://x.example", k) }, map[string]string{EnvGoogleKey: "google-env"}, "", "google-env"},
		{"Gemini explicit wins", func(k string) *Client { return Gemini("https://x.example", k) }, map[string]string{EnvGeminiKey: "gemini-env"}, "gk", "gk"},
		{"no key stays keyless", func(k string) *Client { return OpenAI("http://localhost:11434", k) }, nil, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{EnvOpenAIKey, EnvAnthropicKey, EnvGeminiKey, EnvGoogleKey} {
				t.Setenv(k, tc.env[k])
			}
			t.Setenv(EnvBaseURL, "http://relay-env.example")
			t.Setenv(EnvAPIKey, "rk-relay")
			t.Setenv(EnvHeaders, "X-Relay-Only=1")
			c := tc.build(tc.key)
			if c.apiKey != tc.want {
				t.Errorf("key = %q, want %q", c.apiKey, tc.want)
			}
			if c.configErr != nil || c.relay != nil || c.baseURL == "http://relay-env.example" || c.headers["X-Relay-Only"] != "" {
				t.Errorf("relay configuration leaked into a vendor client: err %v relay %v base %q headers %v", c.configErr, c.relay, c.baseURL, c.headers)
			}
		})
	}
}

// A keyless client sends no auth header at all, not an empty one.
func TestVendorClients_KeylessSendsNoAuthHeader(t *testing.T) {
	t.Setenv(EnvOpenAIKey, "")
	rec := newRecorder(t, http.StatusOK, ccReply)
	if _, err := OpenAI(rec.srv.URL, "").Generate(context.Background(), sampleReq()); err != nil {
		t.Fatal(err)
	}
	if got, present := rec.only(t).Header["Authorization"]; present {
		t.Errorf("Authorization = %q, want the header absent", got)
	}
}

func TestAdapterRegistry(t *testing.T) {
	want := map[string]struct {
		path, provider string
		auth           Auth
		keyEnv         []string
	}{
		"openai":           {"/v1/chat/completions", "openai", Auth{Header: "Authorization", Scheme: "Bearer"}, []string{EnvOpenAIKey}},
		"openai_responses": {"/v1/responses", "openai", Auth{Header: "Authorization", Scheme: "Bearer"}, []string{EnvOpenAIKey}},
		"anthropic":        {"/v1/messages", "anthropic", Auth{Header: "x-api-key"}, []string{EnvAnthropicKey}},
		"gemini":           {"", "gcp.gemini", Auth{Header: "x-goog-api-key"}, []string{EnvGeminiKey, EnvGoogleKey}},
	}
	names := KnownAdapterNames()
	if len(names) != len(want) {
		t.Errorf("KnownAdapterNames = %v, want the %d registered adapters", names, len(want))
	}
	for _, name := range names {
		a, ok := AdapterByName(name)
		w, known := want[name]
		if !ok || !known {
			t.Errorf("adapter %q: registered %v, expected %v", name, ok, known)
			continue
		}
		if a.translator == nil || a.path != w.path || a.provider != w.provider || a.auth != w.auth || !cmp.Equal(a.keyEnv, w.keyEnv) || (a.pathFn != nil) != (w.path == "") {
			t.Errorf("adapter %q = path %q provider %q auth %+v keyEnv %v pathFn set %v", name, a.path, a.provider, a.auth, a.keyEnv, a.pathFn != nil)
		}
	}
	if _, ok := AdapterByName("cohere"); ok {
		t.Error("AdapterByName(cohere) = ok, want an unknown adapter")
	}
}
