package batch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
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

	f := runnerFixture(t, up.URL)
	events := make(usageCapture, 1)
	reg := lifecycle.New()
	reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
	reg.RegisterCollector(events)
	f.rn.Pipeline.Lifecycle = reg

	k := f.key
	if _, _, err := f.rn.Run(context.Background(), "item", k.Spec.KeyHash, TokenClaims{}, f.keyAttr(), adapters.OpenAI, []byte(`{"model":"test-model"}`)); err != nil {
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
