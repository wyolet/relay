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
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi/inference"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
	pkgrelay "github.com/wyolet/relay/sdk/v1"
)

// lister satisfies every catalog.*Lister — each is List(ctx) ([]*T, error).
type lister[T any] []*T

func (l lister[T]) List(context.Context) ([]*T, error) { return l, nil }

type catSnapReader struct{ cat *appcatalog.Catalog }

func (r catSnapReader) Policy(_ context.Context, id string) (*policy.Policy, bool) {
	return r.cat.Current().Policy(id)
}
func (r catSnapReader) RateLimit(_ context.Context, id string) (*ratelimit.RateLimit, bool) {
	return r.cat.Current().RateLimit(id)
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

	cat := appcatalog.New(
		lister[provider.Provider]{prov},
		lister[host.Host]{h},
		lister[policy.Policy]{pol},
		lister[model.Model]{m},
		lister[hostkey.HostKey]{hk},
		lister[ratelimit.RateLimit]{},
		lister[key.Key]{},
		lister[pricing.Pricing]{},
		lister[binding.Binding]{b},
	)
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

	attr := Attribution{TeamID: teamID, CredentialKind: inference.CredentialToken, CredentialID: jti}
	_, _, err := rn.Run(ctx, "item-1", "", policyID, jti, attr, adapters.OpenAI, []byte(`{"model":"test-model"}`))
	if !errors.Is(err, pkgratelimit.ErrRevoked) {
		t.Fatalf("err = %v, want ErrRevoked", err)
	}

	// A live (unrevoked) token gets past the reservation and only then fails
	// on the unreachable upstream — the check is that token's, not blanket.
	_, _, err = rn.Run(ctx, "item-2", "", policyID, "jti-live", attr, adapters.OpenAI, []byte(`{"model":"test-model"}`))
	if errors.Is(err, pkgratelimit.ErrRevoked) {
		t.Fatalf("unrevoked token refused: %v", err)
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
