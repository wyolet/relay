package batch

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/revocation"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	pkgrelay "github.com/wyolet/relay/sdk/v1"
)

// fixture is a Runner over published plans: one model served at one host through a system-owned credential, one user and that user's key.
type fixture struct {
	rn      *Runner
	revoked *revocation.List
	userID  string
	key     *key.Key
}

// keyAttr and tokenAttr are submissions by the fixture's key and its user.
func (f *fixture) keyAttr() Attribution {
	return Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: f.userID, CredentialKind: inference.CredentialKey, CredentialID: f.key.Meta.ID}
}

func (f *fixture) tokenAttr(jti string) Attribution {
	return Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: f.userID, CredentialKind: inference.CredentialToken, CredentialID: jti}
}

// runnerFixture wires a Runner whose pipeline counts on kv.Mem, against a host at baseURL.
func runnerFixture(t *testing.T, baseURL string) *fixture {
	t.Helper()
	var w plantest.World
	prov := w.Provider("vendor")
	h := w.Host("vendor", baseURL)
	m := w.Model(prov, "test-model")
	w.Bind(m, h, adapters.OpenAI)
	w.Credential(h, nil, plantest.System, "sk-test")
	u := w.User()
	k := w.UserKey(u, "rk-secret")
	k.Meta.Name = "rk"
	plans := w.Load(t)

	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	revoked := revocation.New(mem)

	spec := (&adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: "/v1/chat/completions",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  pkgrelay.IdentityTranslator{},
	}).Build()
	return &fixture{
		rn: &Runner{
			Auth: &inference.Auth{Plans: plans.Holder, Revoked: revoked.Set()},
			Pipeline: &pipeline.Pipeline{
				Limiter:  lua.New(pkgratelimit.New(mem, slog.Default(), nil)),
				Selector: keypool.New(mem, slog.Default(), nil, nil),
				Logger:   slog.Default(),
			},
			Specs: adapter.NewRegistry(spec),
		},
		revoked: revoked,
		userID:  u.ID,
		key:     k,
	}
}

// A token revoked after submit stops its already-queued items, from the in-memory revoked set; an unrevoked token gets past authentication and only then fails on the unreachable upstream.
func TestRunRevokedTokenItemFails(t *testing.T) {
	f := runnerFixture(t, "http://upstream.invalid")
	ctx := context.Background()
	if err := f.revoked.Revoke(ctx, "jti-1", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	body := []byte(`{"model":"test-model"}`)

	_, _, err := f.rn.Run(ctx, "item-1", "", TokenClaims{JTI: "jti-1"}, f.tokenAttr("jti-1"), adapters.OpenAI, body)
	if !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("err = %v, want ErrCredentialInvalid", err)
	}
	_, _, err = f.rn.Run(ctx, "item-2", "", TokenClaims{JTI: "jti-live"}, f.tokenAttr("jti-live"), adapters.OpenAI, body)
	if err == nil || errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("unrevoked token: err = %v, want the upstream failure", err)
	}
}

// A queued item runs under the credential that submitted it, so that credential has to still be valid when the item runs — the same checks a long-lived WebSocket repeats per frame.
func TestRunRefusesItemsWhoseCredentialLapsed(t *testing.T) {
	f := runnerFixture(t, "http://upstream.invalid")
	ctx := context.Background()
	body := []byte(`{"model":"test-model"}`)
	cases := map[string]struct {
		hash string
		tok  TokenClaims
		attr Attribution
	}{
		"key no longer published":      {hash: "gone-key-hash", attr: f.keyAttr()},
		"token from before revoke-all": {tok: TokenClaims{JTI: "j", Version: 7}, attr: f.tokenAttr("j")},
		"token past its expiry":        {tok: TokenClaims{JTI: "j", Expires: time.Now().Add(-time.Minute).Unix()}, attr: f.tokenAttr("j")},
	}
	for name, tc := range cases {
		if _, _, err := f.rn.Run(ctx, "item", tc.hash, tc.tok, tc.attr, adapters.OpenAI, body); !errors.Is(err, ErrCredentialInvalid) {
			t.Errorf("%s: err = %v, want ErrCredentialInvalid", name, err)
		}
	}
	if _, _, err := f.rn.Run(ctx, "item", f.key.Spec.KeyHash, TokenClaims{}, f.keyAttr(), adapters.OpenAI, body); errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("live key refused: %v", err)
	}
}

// TestSubmitCarriesTokenJTI: the jti has to leave the submission on the job,
// or the runner has nothing to check when the item finally runs.
func TestSubmitCarriesTokenJTI(t *testing.T) {
	tok := &Caller{PolicyID: "p1"}
	tok.CredentialKind, tok.CredentialID = inference.CredentialToken, "jti-1"
	if got := tok.TokenJTI(); got != "jti-1" {
		t.Errorf("token caller jti = %q, want jti-1", got)
	}
	k := &Caller{KeyHash: "h"}
	k.CredentialKind, k.CredentialID = inference.CredentialKey, "key-id"
	if got := k.TokenJTI(); got != "" {
		t.Errorf("key caller jti = %q, want empty", got)
	}
}
