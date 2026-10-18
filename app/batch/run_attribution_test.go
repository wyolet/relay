package batch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/lifecycle"
)

// A key-submitted item's usage event names the key by its slug, so the row stays readable after the key is renamed or deleted.
func TestRun_StampsTheKeyName(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer up.Close()

	rn, _, policyID := runnerFixture(t)
	h := *rn.Catalog.Current().Hosts()[0]
	h.Spec = host.Spec{BaseURL: up.URL, NoAuth: true}
	if err := rn.Catalog.ApplyHostUpsert(&h); err != nil {
		t.Fatalf("host upsert: %v", err)
	}
	events := make(usageCapture, 1)
	reg := lifecycle.New()
	reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
	reg.RegisterCollector(events)
	rn.Pipeline.Lifecycle = reg

	k, _ := rn.Catalog.Current().KeyByHash(fixtureKeyHash)
	attr := keyAttr()
	attr.CredentialID = k.Meta.ID
	if _, _, err := rn.Run(context.Background(), "item", fixtureKeyHash, policyID, TokenClaims{}, attr, adapters.OpenAI, []byte(`{"model":"test-model"}`)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case ev := <-events:
		if ev.RelayKey != "rk" || ev.CredentialID != k.Meta.ID {
			t.Errorf("event relay_key=%q credential_id=%q, want rk / %s", ev.RelayKey, ev.CredentialID, k.Meta.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no usage event emitted")
	}
}
