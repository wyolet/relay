package onepassword

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	onepassword "github.com/1password/onepassword-sdk-go"

	"github.com/wyolet/relay/pkg/secret"
)

type mockSecrets struct {
	value string
	err   error
	calls int32
}

func (m *mockSecrets) Resolve(_ context.Context, secretReference string) (string, error) {
	atomic.AddInt32(&m.calls, 1)
	if m.err != nil {
		return "", m.err
	}
	if secretReference != "op://Prod/openai/credential" {
		return "", errors.New("unexpected reference: " + secretReference)
	}
	return m.value, nil
}

func TestValidatePath(t *testing.T) {
	for _, path := range []string{
		"op://Prod/openai/credential",
		"op://vault/item/field",
	} {
		if err := validatePath(path); err != nil {
			t.Errorf("validatePath(%q): %v", path, err)
		}
	}

	for _, path := range []string{
		"",
		"Prod/openai/credential",
		"https://example.com/secret",
		"op://",
		"  ",
	} {
		if err := validatePath(path); err == nil {
			t.Errorf("validatePath(%q): want error", path)
		}
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "")
	if _, ok := ConfigFromEnv(); ok {
		t.Fatal("empty token should not register")
	}

	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "ops_test_token")
	cfg, ok := ConfigFromEnv()
	if !ok || cfg.ServiceAccountToken != "ops_test_token" {
		t.Fatalf("cfg: %+v ok=%v", cfg, ok)
	}
}

func TestResolver_WrongKind(t *testing.T) {
	r := New(Config{ServiceAccountToken: "tok"})
	_, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindAWS, Path: "op://v/i/f"})
	if err == nil || !strings.Contains(err.Error(), "wrong kind") {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestResolver_InvalidPath(t *testing.T) {
	r := New(Config{ServiceAccountToken: "tok"})
	_, err := r.Resolve(context.Background(), secret.Ref{Kind: secret.KindOnePassword, Path: "not-a-ref"})
	if err == nil || !strings.Contains(err.Error(), "op://") {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestResolver_ResolveWithMock(t *testing.T) {
	mock := &mockSecrets{value: "sk-live-key"}
	r := &Resolver{
		cfg: Config{ServiceAccountToken: "tok"},
	}
	r.newSecrets = func(context.Context) (secretsAPI, error) {
		return mock, nil
	}

	got, err := r.Resolve(context.Background(), secret.Ref{
		Kind: secret.KindOnePassword,
		Path: "op://Prod/openai/credential",
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != "sk-live-key" {
		t.Fatalf("got %q", got)
	}
	if atomic.LoadInt32(&mock.calls) != 1 {
		t.Fatalf("calls = %d, want 1", mock.calls)
	}

	got, err = r.Resolve(context.Background(), secret.Ref{
		Kind: secret.KindOnePassword,
		Path: "op://Prod/openai/credential",
	})
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if string(got) != "sk-live-key" {
		t.Fatalf("second got %q", got)
	}
	if atomic.LoadInt32(&mock.calls) != 2 {
		t.Fatalf("calls after reuse = %d, want 2", mock.calls)
	}
}

func TestResolver_LazyClientConstruction(t *testing.T) {
	var created int32
	r := &Resolver{cfg: Config{ServiceAccountToken: "tok"}}
	r.newSecrets = func(context.Context) (secretsAPI, error) {
		atomic.AddInt32(&created, 1)
		return &mockSecrets{value: "x"}, nil
	}

	if err := r.ensureClient(context.Background()); err != nil {
		t.Fatalf("ensureClient: %v", err)
	}
	if err := r.ensureClient(context.Background()); err != nil {
		t.Fatalf("second ensureClient: %v", err)
	}
	if atomic.LoadInt32(&created) != 1 {
		t.Fatalf("client created %d times, want 1", created)
	}
}

func TestMapResolveError(t *testing.T) {
	path := "op://Prod/openai/credential"

	cases := []struct {
		err      error
		contains string
	}{
		{errors.New("item not found in vault"), "not found"},
		{&onepassword.RateLimitExceededError{}, "rate limit"},
		{errors.New("invalid secret reference syntax"), "invalid secret reference"},
		{errors.New("unauthorized access"), "authentication failed"},
	}
	for _, tc := range cases {
		err := mapResolveError(path, tc.err)
		if err == nil || !strings.Contains(err.Error(), tc.contains) {
			t.Fatalf("mapResolveError(%v): %v, want substring %q", tc.err, err, tc.contains)
		}
	}
}

func TestResolver_LiveResolve(t *testing.T) {
	token := strings.TrimSpace(os.Getenv("OP_SERVICE_ACCOUNT_TOKEN"))
	if token == "" {
		t.Skip("OP_SERVICE_ACCOUNT_TOKEN not set")
	}
	refPath := strings.TrimSpace(os.Getenv("OP_TEST_SECRET_REF"))
	if refPath == "" {
		refPath = "op://Prod/openai/credential"
	}

	r := New(Config{ServiceAccountToken: token})
	got, err := r.Resolve(context.Background(), secret.Ref{
		Kind: secret.KindOnePassword,
		Path: refPath,
	})
	if err != nil {
		t.Fatalf("live Resolve: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("live resolve returned empty value")
	}
}
