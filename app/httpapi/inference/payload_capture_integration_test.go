//go:build integration

// payload_capture_integration_test.go covers the capture decision for a key no policy governs, on the runners that need the policy-less flow: its switch, settings.Inference.AllowMissingPolicy, is reachable only through Hydrate.
// Run with: make test-integration.

package inference

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/settings"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/internal/storage/storagetest"
	"github.com/wyolet/relay/pkg/slug"
)

// policylessCaptureCatalog seeds a keyless host at upstreamURL serving one model, and two personal keys with no policy, one capturing and one not. It returns the hydrated catalog, the model name, and each key's plaintext by its flag.
func policylessCaptureCatalog(t *testing.T, upstreamURL string) (*appcatalog.Catalog, string, map[bool]string) {
	t.Helper()
	ctx := context.Background()
	pool := storagetest.Pool(t)
	cat, stores, err := appcatalog.BootstrapStores(ctx, appcatalog.BootstrapOptions{Pool: pool})
	if err != nil {
		t.Fatalf("BootstrapStores: %v", err)
	}

	sfx := suffix()
	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "acme-" + sfx, Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	if err := stores.Provider.Upsert(ctx, prov); err != nil {
		t.Fatalf("upsert provider: %v", err)
	}
	h := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "open-" + sfx, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: upstreamURL, NoAuth: true},
	}
	if err := stores.Host.Upsert(ctx, h); err != nil {
		t.Fatalf("upsert host: %v", err)
	}
	name := "capture-" + sfx
	md := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name, Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From(name)}}, Pointer: slug.From(name)},
	}
	if err := stores.Model.Upsert(ctx, md); err != nil {
		t.Fatalf("upsert model: %v", err)
	}
	b := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: name + "-on-open", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: md.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI},
	}
	if err := stores.Binding.Upsert(ctx, b); err != nil {
		t.Fatalf("upsert binding: %v", err)
	}

	u := &user.User{ID: meta.NewID(), Username: "capture-" + sfx}
	if err := user.NewStore(gen.New(pool)).Upsert(ctx, u); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	plaintexts := map[bool]string{}
	for _, captures := range []bool{true, false} {
		g, err := key.Generate()
		if err != nil {
			t.Fatalf("key.Generate: %v", err)
		}
		k := &key.Key{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "capture-" + strconv.FormatBool(captures) + "-" + sfx, Owner: meta.Owner{Kind: meta.OwnerUser, ID: u.ID}},
			Spec: key.Spec{
				Principal:             key.Principal{Kind: key.PrincipalUser, ID: u.ID},
				KeyHash:               g.KeyHash,
				Prefix:                g.Prefix,
				PayloadLoggingEnabled: captures,
			},
		}
		if err := stores.Key.Upsert(ctx, k); err != nil {
			t.Fatalf("upsert key: %v", err)
		}
		plaintexts[captures] = g.Plaintext
	}

	raw, err := json.Marshal(settings.Inference{AllowMissingPolicy: true})
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	if _, err := stores.Settings.Upsert(ctx, settings.SectionInference, raw); err != nil {
		t.Fatalf("upsert settings: %v", err)
	}
	if _, err := cat.Hydrate(ctx, stores, appcatalog.BootstrapOptions{Pool: pool}); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	return cat, name, plaintexts
}

// With no policy, the key's own flag decides, on the pipeline and on a WebSocket frame.
func TestIntegration_PayloadCaptureWithoutPolicyFollowsTheKey(t *testing.T) {
	cat, modelName, plaintexts := policylessCaptureCatalog(t, okUpstream(t).URL)
	body := captureBodyFor(modelName)
	for _, captures := range []bool{true, false} {
		d, gates := captureDeps(t, cat)
		postThroughPipeline(t, d, plaintexts[captures], body)
		if got := awaitGate(t, gates); got != captures {
			t.Errorf("pipeline, key flag %v: captured = %v", captures, got)
		}
		sendWSFrame(t, d, plaintexts[captures], body)
		if got := awaitGate(t, gates); got != captures {
			t.Errorf("websocket frame, key flag %v: captured = %v", captures, got)
		}
	}
}
