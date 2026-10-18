package secret

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRefValidate(t *testing.T) {
	bad := []Ref{
		{Kind: KindEnv},          // missing env
		{Kind: KindStored},       // missing id
		{Kind: "vault", ID: "x"}, // unknown kind
		{Kind: KindEnv, ID: "x"}, // env without env name
		{Kind: KindAWS},          // path-addressed, missing path
		{Kind: KindBitwarden},    // path-addressed, missing path
		{Kind: KindGCP},          // path-addressed, missing path
		{Kind: KindAzure},        // path-addressed, missing path
		{Kind: KindOnePassword},  // path-addressed, missing path
	}
	for _, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("Validate(%+v): want error", r)
		}
	}
	good := []Ref{
		{Kind: KindEnv, Env: "FOO"},
		{Kind: KindStored, ID: "abc"},
		{Kind: KindAWS, Path: "prod/openai-key:apiKey"},
		{Kind: KindBitwarden, Path: "openai-key/password"},
		{Kind: KindGCP, Path: "openai-key"},
		{Kind: KindGCP, Path: "openai-key:3"},
		{Kind: KindOnePassword, Path: "op://Prod/openai/credential"},
	}
	for _, r := range good {
		if err := r.Validate(); err != nil {
			t.Errorf("Validate(%+v): %v", r, err)
		}
	}
}

func TestRefValidateErrorText(t *testing.T) {
	cases := map[string]Ref{
		"secret: env ref requires a non-empty env var name": {Kind: KindEnv},
		"secret: stored ref requires a non-empty id":        {Kind: KindStored},
		"secret: oauth ref requires a non-empty id":         {Kind: KindOAuth, Provider: "p"},
		"secret: oauth ref requires a non-empty provider":   {Kind: KindOAuth, ID: "x"},
		"secret: aws ref requires a non-empty path":         {Kind: KindAWS},
		`secret: unknown kind "vault"`:                      {Kind: "vault"},
	}
	for want, r := range cases {
		err := r.Validate()
		if err == nil || err.Error() != want {
			t.Errorf("Validate(%+v) = %v, want %q", r, err, want)
		}
	}
}

// Refs are persisted, so the JSON field names must not drift.
func TestRefJSONShape(t *testing.T) {
	r := Ref{Kind: KindOAuth, Env: "E", ID: "i", Path: "p", Provider: "pr"}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"kind":"oauth","env":"E","id":"i","path":"p","provider":"pr"}`
	if string(b) != want {
		t.Fatalf("json = %s, want %s", b, want)
	}
	b, _ = json.Marshal(Ref{Kind: KindStored, ID: "x"})
	if string(b) != `{"kind":"stored","id":"x"}` {
		t.Fatalf("omitempty json = %s", b)
	}
}

func TestEnvResolver(t *testing.T) {
	t.Setenv("MY_SECRET", "s3kr3t")
	reg := NewRegistry()
	reg.Register(KindEnv, EnvResolver{})

	got, err := reg.Resolve(context.Background(), Ref{Kind: KindEnv, Env: "MY_SECRET"})
	if err != nil || string(got) != "s3kr3t" {
		t.Fatalf("env resolve: got %q err %v", got, err)
	}
	// Unset → loud error.
	if _, err := reg.Resolve(context.Background(), Ref{Kind: KindEnv, Env: "NOPE_UNSET"}); err == nil {
		t.Fatal("unset env should error")
	}
	if _, err := (EnvResolver{}).Resolve(context.Background(), Ref{Kind: KindStored, ID: "x"}); err == nil {
		t.Fatal("wrong kind should error")
	}
}

func TestRegistry_UnknownKind(t *testing.T) {
	reg := NewRegistry()
	reg.Register(KindEnv, EnvResolver{})
	// stored not registered → clear error.
	_, err := reg.Resolve(context.Background(), Ref{Kind: KindStored, ID: "x"})
	if err == nil || err.Error() != `secret: no resolver registered for kind "stored"` {
		t.Fatalf("want unregistered-kind error, got %v", err)
	}
}

func TestRegistry_InvalidRef(t *testing.T) {
	reg := NewRegistry()
	reg.Register(KindEnv, EnvResolver{})
	if _, err := reg.Resolve(context.Background(), Ref{Kind: KindEnv}); err == nil {
		t.Fatal("want validation error")
	}
}

type expiringResolver struct{ at time.Time }

func (e expiringResolver) Resolve(ctx context.Context, ref Ref) ([]byte, error) {
	c, err := e.ResolveCredential(ctx, ref)
	return c.Value, err
}

func (e expiringResolver) ResolveCredential(context.Context, Ref) (Credential, error) {
	return Credential{Value: []byte("tok"), ExpiresAt: e.at}, nil
}

func TestCredentialResolverIsOptional(t *testing.T) {
	var r Resolver = expiringResolver{at: time.Unix(1, 0)}
	cr, ok := r.(CredentialResolver)
	if !ok {
		t.Fatal("expiringResolver should satisfy CredentialResolver")
	}
	c, err := cr.ResolveCredential(context.Background(), Ref{})
	if err != nil || string(c.Value) != "tok" || !c.ExpiresAt.Equal(time.Unix(1, 0)) {
		t.Fatalf("credential = %+v, %v", c, err)
	}
	if _, ok := Resolver(EnvResolver{}).(CredentialResolver); ok {
		t.Fatal("EnvResolver values do not expire")
	}
}
