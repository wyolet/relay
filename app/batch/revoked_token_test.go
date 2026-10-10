package batch

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
	pkgrelay "github.com/wyolet/relay/sdk/v1"
)

type catSnapReader struct{ cat *appcatalog.Catalog }

func (r catSnapReader) Policy(_ context.Context, id string) (*policy.Policy, bool) {
	return r.cat.Current().Policy(id)
}
func (r catSnapReader) RateLimit(_ context.Context, id string) (*ratelimit.RateLimit, bool) {
	return r.cat.Current().RateLimit(id)
}

// The fixture's one live credential: a personal key, and the user's token
// version a token submission is checked against.
const (
	fixtureKeyHash = "fixture-key-hash"
	fixtureUser    = "u-1"
)

type fixtureVersions map[string]int

func (v fixtureVersions) TokenVersions(context.Context) (map[string]int, error) { return v, nil }

// keyAttr and tokenAttr are submissions by the fixture's key and its user.
func keyAttr() Attribution {
	return Attribution{PrincipalKind: string(key.PrincipalUser), PrincipalID: fixtureUser, CredentialKind: inference.CredentialKey}
}

func tokenAttr(teamID, jti string) Attribution {
	return Attribution{TeamID: teamID, PrincipalKind: string(key.PrincipalUser), PrincipalID: fixtureUser,
		CredentialKind: inference.CredentialToken, CredentialID: jti}
}

// runnerFixture wires a Runner over an in-memory catalog and a pipeline whose
// reservation runs against kv.Mem, so a batch item reaches the same inbound
// Reserve a live request does.
func runnerFixture(t *testing.T) (*Runner, kv.Store, string) {
	t.Helper()
	provID, hostID, hkID, modID, polID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()

	prov := &provider.Provider{Meta: meta.Metadata{ID: provID, Name: "vendor", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h := &host.Host{
		Meta: meta.Metadata{ID: hostID, Name: "vendor", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: "http://upstream.invalid", NoAuth: true},
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: hkID, Name: "hk", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: hostkey.Spec{HostID: hostID, PolicyID: polID, Value: "sk-test", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	m := &model.Model{
		Meta: meta.Metadata{ID: modID, Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From("test-model")}}, Pointer: slug.From("test-model")},
	}
	b := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "test-model-binding", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.OpenAI},
	}
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
		Spec: policy.Spec{ModelIDs: []string{modID}, HostKeyIDs: []string{hkID}},
	}

	rk := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rk", Owner: meta.Owner{Kind: meta.OwnerUser, ID: fixtureUser}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: fixtureUser}, KeyHash: fixtureKeyHash},
	}
	cat := catalogtest.Catalog{
		Providers: []*provider.Provider{prov},
		Hosts:     []*host.Host{h},
		Policies:  []*policy.Policy{pol},
		Models:    []*model.Model{m},
		HostKeys:  []*hostkey.HostKey{hk},
		Keys:      []*key.Key{rk},
		Bindings:  []*binding.Binding{b},
	}.New()
	cat.UseTokenVersions(fixtureVersions{fixtureUser: 0})
	if err := cat.Reload(t.Context()); err != nil {
		t.Fatalf("catalog reload: %v", err)
	}

	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	svc := policy.NewService(catSnapReader{cat: cat}, keypool.New(mem, slog.Default(), nil, nil), pkgratelimit.New(mem, slog.Default(), nil))

	spec := (&adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: "/v1/chat/completions",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  pkgrelay.IdentityTranslator{},
	}).Build()

	return &Runner{
		Resolver: routing.New(cat),
		Pipeline: &pipeline.Pipeline{Policy: svc, Logger: slog.Default()},
		Specs:    adapter.NewRegistry(spec),
		Catalog:  cat,
	}, mem, polID
}

// TestRunRevokedTokenItemFails: a token revoked after submit must stop its
// already-queued items, which only happens if the runner carries the jti the
// inbound reservation checks.
func TestRunRevokedTokenItemFails(t *testing.T) {
	rn, mem, policyID := runnerFixture(t)
	const teamID, jti = "team-1", "jti-1"
	ctx := context.Background()
	if err := mem.Set(ctx, policy.RevokedKey(teamID, jti), []byte("1"), time.Hour); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, _, err := rn.Run(ctx, "item-1", "", policyID, TokenClaims{JTI: jti}, tokenAttr(teamID, jti), adapters.OpenAI, []byte(`{"model":"test-model"}`))
	if !errors.Is(err, pkgratelimit.ErrRevoked) {
		t.Fatalf("err = %v, want ErrRevoked", err)
	}

	// A live (unrevoked) token gets past the reservation and only then fails
	// on the unreachable upstream — the check is that token's, not blanket.
	_, _, err = rn.Run(ctx, "item-2", "", policyID, TokenClaims{JTI: "jti-live"}, tokenAttr(teamID, "jti-live"), adapters.OpenAI, []byte(`{"model":"test-model"}`))
	if errors.Is(err, pkgratelimit.ErrRevoked) {
		t.Fatalf("unrevoked token refused: %v", err)
	}
}

// A queued item runs under the credential that submitted it, so that
// credential has to still be valid when the item runs — the same checks a
// long-lived WebSocket repeats per frame.
func TestRunRefusesItemsWhoseCredentialLapsed(t *testing.T) {
	rn, _, policyID := runnerFixture(t)
	ctx := context.Background()
	body := []byte(`{"model":"test-model"}`)
	cases := map[string]struct {
		hash string
		tok  TokenClaims
		attr Attribution
	}{
		"key no longer in the catalog": {hash: "gone-key-hash", attr: keyAttr()},
		"token from before revoke-all": {tok: TokenClaims{JTI: "j", Version: 7}, attr: tokenAttr("", "j")},
		"token past its expiry":        {tok: TokenClaims{JTI: "j", Expires: time.Now().Add(-time.Minute).Unix()}, attr: tokenAttr("", "j")},
	}
	for name, tc := range cases {
		if _, _, err := rn.Run(ctx, "item", tc.hash, policyID, tc.tok, tc.attr, adapters.OpenAI, body); !errors.Is(err, ErrCredentialInvalid) {
			t.Errorf("%s: err = %v, want ErrCredentialInvalid", name, err)
		}
	}
	if _, _, err := rn.Run(ctx, "item", fixtureKeyHash, policyID, TokenClaims{}, keyAttr(), adapters.OpenAI, body); errors.Is(err, ErrCredentialInvalid) {
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
