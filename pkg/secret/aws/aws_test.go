package aws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/pkg/secret"
)

func TestParsePath(t *testing.T) {
	name, key, err := parsePath("prod/openai:apiKey")
	if err != nil || name != "prod/openai" || key != "apiKey" {
		t.Fatalf("got %q %q err %v", name, key, err)
	}
	name, key, err = parsePath("plain-secret")
	if err != nil || name != "plain-secret" || key != "" {
		t.Fatalf("plain: %q %q err %v", name, key, err)
	}
	for _, p := range []string{"", ":onlykey", "name:"} {
		if _, _, err := parsePath(p); err == nil {
			t.Fatalf("parsePath(%q): want error", p)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_ACCESS_KEY_ID", "AKID")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SECRET")
	t.Setenv("AWS_SESSION_TOKEN", "TOKEN")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Region != "us-west-2" || cfg.Credentials.AccessKeyID != "AKID" ||
		cfg.Credentials.SecretAccessKey != "SECRET" || cfg.Credentials.SessionToken != "TOKEN" {
		t.Fatalf("cfg: %+v", cfg)
	}
}

func TestResolver_PlainSecretString(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Target") != targetGet {
			http.Error(w, "bad target", http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256") {
			http.Error(w, "unsigned", http.StatusUnauthorized)
			return
		}
		var req getSecretRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.SecretID != "my-secret" {
			http.Error(w, "bad secret id", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(getSecretResponse{
			SecretString: ptr("upstream-api-key"),
		})
	}))
	defer srv.Close()

	res := New(testConfig(testHost(t, srv)))
	got, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAWS, Path: "my-secret"})
	if err != nil || string(got) != "upstream-api-key" {
		t.Fatalf("resolve: %q err %v", got, err)
	}
}

func TestResolver_JSONKeyExtraction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req getSecretRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.SecretID != "prod/openai" {
			http.Error(w, "wrong id", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(getSecretResponse{
			SecretString: ptr(`{"apiKey":"sk-live","org":"acme"}`),
		})
	}))
	defer srv.Close()

	res := New(testConfig(testHost(t, srv)))
	ref := secret.Ref{Kind: secret.KindAWS, Path: "prod/openai:apiKey"}
	got, err := res.Resolve(context.Background(), ref)
	if err != nil || string(got) != "sk-live" {
		t.Fatalf("resolve: %q err %v", got, err)
	}
}

func TestResolver_SecretBinary(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(getSecretResponse{
			SecretBinary: ptr("aGVsbG8="),
		})
	}))
	defer srv.Close()

	res := New(testConfig(testHost(t, srv)))
	got, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAWS, Path: "bin"})
	if err != nil || string(got) != "hello" {
		t.Fatalf("resolve: %q err %v", got, err)
	}
}

func TestResolver_SessionTokenHeader(t *testing.T) {
	var seenToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenToken = r.Header.Get("X-Amz-Security-Token")
		_ = json.NewEncoder(w).Encode(getSecretResponse{SecretString: ptr("x")})
	}))
	defer srv.Close()

	cfg := testConfig(testHost(t, srv))
	cfg.Credentials.SessionToken = "sess-tok"
	res := New(cfg)
	if _, err := res.Resolve(context.Background(), secret.Ref{Kind: secret.KindAWS, Path: "s"}); err != nil {
		t.Fatal(err)
	}
	if seenToken != "sess-tok" {
		t.Fatalf("token header: %q", seenToken)
	}
}

func TestResolver_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req getSecretRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.SecretID == "plain" {
			_ = json.NewEncoder(w).Encode(getSecretResponse{SecretString: ptr("not-json-body")})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"__type":"ResourceNotFoundException","Message":"secret not found"}`))
	}))
	defer srv.Close()

	res := New(testConfig(testHost(t, srv)))

	cases := []struct {
		ref secret.Ref
		sub string
	}{
		{secret.Ref{Kind: secret.KindEnv, Path: "x"}, "wrong kind"},
		{secret.Ref{Kind: secret.KindAWS, Path: "missing"}, "secret not found"},
		{secret.Ref{Kind: secret.KindAWS, Path: "plain:missingKey"}, "not valid JSON"},
	}
	for _, tc := range cases {
		_, err := res.Resolve(context.Background(), tc.ref)
		if err == nil || !strings.Contains(err.Error(), tc.sub) {
			t.Fatalf("ref %+v: got err %v want %q", tc.ref, err, tc.sub)
		}
	}

	// JSON key missing in object
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(getSecretResponse{SecretString: ptr(`{"other":"v"}`)})
	}))
	defer srv2.Close()
	res2 := New(testConfig(testHost(t, srv2)))
	_, err := res2.Resolve(context.Background(), secret.Ref{Kind: secret.KindAWS, Path: "s:noKey"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing json key: %v", err)
	}
}

func testHost(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Host
}

func testConfig(host string) Config {
	return Config{
		Region:   "us-east-1",
		Endpoint: "http://" + host,
		Credentials: Credentials{
			AccessKeyID:     "AKID",
			SecretAccessKey: "SECRET",
		},
		HTTPClient: http.DefaultClient,
		Now: func() time.Time {
			return time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		},
	}
}

func ptr[T any](v T) *T { return &v }
