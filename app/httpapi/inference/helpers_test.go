package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/pkg/lifecycle"
)

const captureKeyPlaintext = "sk-wr-capture"

// captureCase sets the key's own payload flag.
type captureCase struct {
	keyCaptures bool
}

// rekey changes the fixture key of a buildDispatchCatalog catalog in the plans and the catalog, and refreshes the principal the test holds.
func rekey(t *testing.T, cat *catalog.Catalog, mutate func(*key.Key)) {
	t.Helper()
	f := fixtureOf(t, cat)
	k := *f.key
	mutate(&k)
	plantest.Apply(t, f.plans, &k)
	if err := cat.ApplyKeyUpsert(&k); err != nil {
		t.Fatalf("key upsert: %v", err)
	}
	f.key = &k
	*f.principal = *f.resolve(t)
}

// captureCatalog is the dispatch fixture pointed at upstreamURL, with its key authenticating as captureKeyPlaintext, allowed to bring its own upstream key, and capturing as c says.
func captureCatalog(t *testing.T, upstreamURL string, c captureCase) *catalog.Catalog {
	t.Helper()
	cat, _ := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	pointHostAt(t, cat, upstreamURL)
	rekey(t, cat, func(k *key.Key) {
		k.Spec.KeyHash = apikey.Hash(captureKeyPlaintext)
		k.Spec.PassthroughAllowed = true
		k.Spec.PayloadLoggingEnabled = c.keyCaptures
	})
	return cat
}

// captureDeps wires runnable deps whose post-flight hook reports each request's capture gate.
func captureDeps(t *testing.T, cat *catalog.Catalog) (Deps, <-chan bool) {
	t.Helper()
	d := buildRunnableDeps(t, cat)
	gates := make(chan bool, 4)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "capture-gate", Fn: func(lc *lifecycle.Context, _ *lifecycle.PostFlightEvent) (any, error) {
		gates <- lc.PayloadLog
		return nil, nil
	}})
	d.Lifecycle = reg
	d.Pipeline.Lifecycle = reg
	d.Proxy = proxy.New(nil, reg, nil)
	return d, gates
}

func okUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return payload.Error.Code
}

// waitForFreeSlot waits for the detached post-flight to hand the admission slot back.
func waitForFreeSlot(t *testing.T, adm *httpapi.Admission) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		probe := lifecycle.NewContext("probe", "pipeline", time.Now())
		if adm.PreFlight(context.Background(), probe, &lifecycle.PreFlightEvent{}) == nil {
			adm.Collect(probe)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("admission slot never released")
}
