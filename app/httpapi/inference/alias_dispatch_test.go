package inference

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/pkg/kv"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// catSnapReader adapts *catalog.Catalog to policy.SnapshotReader (mirrors
// cmd/relay's catalogSnapReader, which lives in the composition root).
type catSnapReader struct{ cat *catalog.Catalog }

func (r catSnapReader) Policy(id string) (*policy.Policy, bool) { return r.cat.Current().Policy(id) }
func (r catSnapReader) RateLimit(id string) (*ratelimit.RateLimit, bool) {
	return r.cat.Current().RateLimit(id)
}

// buildRunnableDeps is buildDeps with a fully-wired pipeline (policy service
// over kv.Mem) so tests can complete a real upstream round-trip.
func buildRunnableDeps(t *testing.T, cat *catalog.Catalog) Deps {
	t.Helper()
	d := buildDeps(t, cat)
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	svc := policy.NewService(catSnapReader{cat: cat}, keypool.New(mem, slog.Default(), nil, nil), pkgratelimit.New(mem, slog.Default(), nil))
	d.Pipeline = &pipeline.Pipeline{Policy: svc, Logger: slog.Default()}
	return d
}

// aliasDispatchCatalog rebuilds the standard dispatch fixture so the host
// points at the given upstream URL with NoAuth (anonymous key — no secret
// resolution in tests) and the model declares an exact + wildcard alias.
func aliasDispatchCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *relaykey.RelayKey) {
	t.Helper()
	cat, rk := buildDispatchCatalog(t, "groq", adapters.OpenAI)
	snap := cat.Current()

	hosts := snap.Hosts()
	if len(hosts) != 1 {
		t.Fatalf("fixture hosts: %d", len(hosts))
	}
	h := *hosts[0]
	h.Spec = host.Spec{BaseURL: upstreamURL, NoAuth: true}
	if err := cat.ApplyHostUpsert(&h); err != nil {
		t.Fatalf("host upsert: %v", err)
	}

	models := snap.ModelsByName("test-model")
	if len(models) != 1 {
		t.Fatalf("fixture models: %d", len(models))
	}
	m := *models[0]
	m.Spec.Aliases = []string{"test-model[1m]", "test-modelx[*]"}
	if err := cat.ApplyModelUpsert(&m); err != nil {
		t.Fatalf("model upsert: %v", err)
	}
	return cat, rk
}

// TestDispatch_AliasBytePass_VerbatimWireName proves the full dispatch path:
// an alias-addressed request byte-passes with the request body's model field
// rewritten to the alias verbatim form — the declared string for an exact
// alias (even when the caller spelled it differently), the caller's raw
// string for a wildcard match.
func TestDispatch_AliasBytePass_VerbatimWireName(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = string(b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	cat, rk := aliasDispatchCatalog(t, up.URL)
	d := buildRunnableDeps(t, cat)

	cases := []struct {
		name      string
		send      string // caller's model string (body + minimal parse)
		wantWire  string // model field the upstream must receive
	}{
		{"exact alias, declared spelling", "test-model[1m]", "test-model[1m]"},
		{"exact alias, caller variant spelling", "TEST-MODEL.1M", "test-model[1m]"},
		{"wildcard alias, raw goes through", "test-modelx[exp-2027]", "test-modelx[exp-2027]"},
		{"no alias, snapshot upstream", "test-model", "test-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			r = withNormalContext(r, rk)
			w := httptest.NewRecorder()

			Dispatch(d, w, r, DispatchInput{
				Inbound:   adapters.OpenAI,
				Body:      []byte(`{"model":"` + tc.send + `","stream":false}`),
				ModelName: tc.send,
				Stream:    false,
			})

			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			mu.Lock()
			body := gotBody
			mu.Unlock()
			want := `"model":"` + tc.wantWire + `"`
			if !strings.Contains(body, want) {
				t.Errorf("upstream body %q does not carry %q", body, want)
			}
		})
	}
}
