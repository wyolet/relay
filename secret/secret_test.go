package secret

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRefValidate(t *testing.T) {
	for _, tc := range []struct {
		ref  Ref
		want string // "" = valid
	}{
		{Ref{Kind: KindEnv, Env: "FOO"}, ""},
		{Ref{Kind: KindStored, ID: "abc"}, ""},
		{Ref{Kind: KindOAuth, ID: "abc", Provider: "p"}, ""},
		{Ref{Kind: KindAWS, Path: "prod/openai-key:apiKey"}, ""},
		{Ref{Kind: KindBitwarden, Path: "openai-key/password"}, ""},
		{Ref{Kind: KindGCP, Path: "openai-key:3"}, ""},
		{Ref{Kind: KindAzure, Path: "openai-key"}, ""},
		{Ref{Kind: KindOnePassword, Path: "op://Prod/openai/credential"}, ""},
		{Ref{Kind: KindEnv}, "secret: env ref requires a non-empty env var name"},
		// Each kind reads its own field only.
		{Ref{Kind: KindEnv, ID: "x", Path: "x"}, "secret: env ref requires a non-empty env var name"},
		{Ref{Kind: KindStored, Env: "x", Path: "x"}, "secret: stored ref requires a non-empty id"},
		{Ref{Kind: KindOAuth, Provider: "p"}, "secret: oauth ref requires a non-empty id"},
		{Ref{Kind: KindOAuth, ID: "x"}, "secret: oauth ref requires a non-empty provider"},
		{Ref{Kind: KindAWS, ID: "x", Env: "x"}, "secret: aws ref requires a non-empty path"},
		{Ref{Kind: KindBitwarden}, "secret: bitwarden ref requires a non-empty path"},
		{Ref{Kind: KindGCP}, "secret: gcp ref requires a non-empty path"},
		{Ref{Kind: KindAzure}, "secret: azure ref requires a non-empty path"},
		{Ref{Kind: KindOnePassword}, "secret: onepassword ref requires a non-empty path"},
		{Ref{Kind: "vault", ID: "x", Path: "x", Env: "x"}, `secret: unknown kind "vault"`},
		{Ref{ID: "x"}, `secret: unknown kind ""`},
		{Ref{Kind: "ENV", Env: "FOO"}, `secret: unknown kind "ENV"`},
	} {
		err := tc.ref.Validate()
		if (tc.want == "") != (err == nil) || (err != nil && err.Error() != tc.want) {
			t.Errorf("Validate(%+v) = %v, want %q", tc.ref, err, tc.want)
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

// recording answers every ref with its own name and remembers the ref.
type recording struct {
	name string
	got  *Ref
}

func (r recording) Resolve(_ context.Context, ref Ref) ([]byte, error) {
	*r.got = ref
	return []byte(r.name), nil
}

func TestRegistry_Resolve(t *testing.T) {
	t.Setenv("MY_SECRET", "s3kr3t")
	t.Setenv("MY_EMPTY", "")
	var seen Ref
	reg := NewRegistry()
	reg.Register(KindEnv, EnvResolver{})
	reg.Register(KindAWS, recording{"first", &seen})
	reg.Register(KindAWS, recording{"aws", &seen})

	for _, tc := range []struct {
		ref           Ref
		want, wantErr string
	}{
		{Ref{Kind: KindEnv, Env: "MY_SECRET"}, "s3kr3t", ""},
		{Ref{Kind: KindAWS, Path: "prod/key"}, "aws", ""},
		{Ref{Kind: KindEnv, Env: "NOPE_UNSET"}, "", `secret/env: "NOPE_UNSET" is unset or empty`},
		{Ref{Kind: KindEnv, Env: "MY_EMPTY"}, "", `secret/env: "MY_EMPTY" is unset or empty`},
		{Ref{Kind: KindStored, ID: "x"}, "", `secret: no resolver registered for kind "stored"`},
		// An invalid ref never reaches its resolver.
		{Ref{Kind: KindAWS}, "", "secret: aws ref requires a non-empty path"},
		{Ref{Kind: KindEnv}, "", "secret: env ref requires a non-empty env var name"},
	} {
		seen = Ref{}
		got, err := reg.Resolve(context.Background(), tc.ref)
		if string(got) != tc.want || (tc.wantErr == "") != (err == nil) || (err != nil && err.Error() != tc.wantErr) {
			t.Errorf("Resolve(%+v) = %q, %v; want %q, %q", tc.ref, got, err, tc.want, tc.wantErr)
		}
		if wantSeen := tc.ref; tc.want == "aws" && seen != wantSeen {
			t.Errorf("resolver got %+v, want %+v", seen, wantSeen)
		} else if tc.want != "aws" && seen != (Ref{}) {
			t.Errorf("Resolve(%+v) reached the aws resolver", tc.ref)
		}
	}

	_, err := EnvResolver{}.Resolve(context.Background(), Ref{Kind: KindStored, ID: "x", Env: "MY_SECRET"})
	if err == nil || err.Error() != `secret/env: wrong kind "stored"` {
		t.Errorf("EnvResolver on a stored ref: %v", err)
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
