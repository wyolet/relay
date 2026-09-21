package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/relaykey"
	"github.com/wyolet/relay/app/tokencount"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/kv"
)

// countProfile is stubProfile (anthropic shape, no User-Agent match) plus the count-tokens route and a session marker.
type countProfile struct{ stubProfile }

func (countProfile) Name() string                    { return "counting" }
func (countProfile) TokenCountPath() string          { return "/v1/messages/count_tokens" }
func (countProfile) SessionKey(h http.Header) string { return h.Get("x-test-session") }

func newCountProfile() clientprofile.Profile { return countProfile{} }

func profileRegistry(p ...clientprofile.Profile) *clientprofile.Registry {
	reg := clientprofile.New()
	for _, one := range p {
		_ = reg.Register(one)
	}
	return reg
}

// countingRegistry mirrors buildTestRegistry's anthropic spec with a CountPath, so its pipeline adapter carries the upstream-counter capability.
func countingRegistry() *adapter.Registry {
	return adapter.NewRegistry((&adapter.Spec{
		Name:        adapters.Anthropic,
		DefaultPath: "/v1/messages",
		CountPath:   "/v1/messages/count_tokens",
		Auth:        adapter.AuthStrategy{Header: "x-api-key"},
		Translator:  stubV1Translator{},
	}).Build())
}

// countCatalog points the fixture host at upstreamURL and drops its auth, so a count reaches the test server with the anonymous key routing injects.
func countCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *relaykey.RelayKey) {
	t.Helper()
	cat, rk := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	h := *cat.Current().Hosts()[0]
	h.Spec = host.Spec{BaseURL: upstreamURL, NoAuth: true}
	if err := cat.ApplyHostUpsert(&h); err != nil {
		t.Fatalf("host upsert: %v", err)
	}
	return cat, rk
}

// countRequest drives the handler the way the mounted route does: classified, authed, with the profile on the context.
func countRequest(d Deps, rk *relaykey.RelayKey, body string, headers http.Header) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/counting/v1/messages/count_tokens", strings.NewReader(body))
	for k, vs := range headers {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	r = withNormalContext(r, rk)
	r = r.WithContext(clientprofile.WithProfile(r.Context(), newCountProfile()))
	w := httptest.NewRecorder()
	handleCountTokens(d, w, r)
	return w
}

func countAnswer(t *testing.T, w *httptest.ResponseRecorder) (int, string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse answer: %v — raw: %s", err, w.Body.String())
	}
	return out.InputTokens, w.Header().Get(httpheader.HeaderTokenCount)
}

// An upstream that counts is asked, and its number is passed through as the exact tier.
func TestCountTokens_ExactTier(t *testing.T) {
	var gotPath, gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&probe)
		gotModel = probe.Model
		_, _ = w.Write([]byte(`{"input_tokens":777}`))
	}))
	defer up.Close()

	cat, rk := countCatalog(t, up.URL)
	d := buildRunnableDeps(t, cat)
	d.Specs = countingRegistry()
	d.Adapters = d.Specs.AdapterMap()
	d.Profiles = profileRegistry(newCountProfile())

	count, tier := countAnswer(t, countRequest(d, rk, `{"model":"test-model","messages":[]}`, nil))
	if count != 777 || tier != "exact" {
		t.Fatalf("count = %d, tier = %q, want 777/exact", count, tier)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotModel != "test-model" {
		t.Errorf("upstream model = %q, want the binding's upstream name", gotModel)
	}
}

// With no upstream counter but a ratio measured for this model, the answer is the body length scaled by it.
func TestCountTokens_CalibratedTier(t *testing.T) {
	cat, rk := countCatalog(t, "http://upstream.invalid")
	d := buildRunnableDeps(t, cat) // buildTestRegistry's anthropic spec has no CountPath
	d.Profiles = profileRegistry(newCountProfile())

	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	cal := tokencount.NewCalibrator(mem)
	d.TokenCalibrator = cal

	modelID := cat.Current().ModelsByName("test-model")[0].Meta.ID
	cal.Observe(t.Context(), "", modelID, 1000, 500) // 0.5 tokens per byte

	body := `{"model":"test-model","messages":[]}`
	count, tier := countAnswer(t, countRequest(d, rk, body, nil))
	if tier != "calibrated" {
		t.Fatalf("tier = %q, want calibrated", tier)
	}
	if want := len(body) / 2; count != want {
		t.Fatalf("count = %d, want %d (body bytes × ratio)", count, want)
	}

	// The profile names the conversation, and its own ratio is the sharper answer the handler must prefer.
	cal.Observe(t.Context(), "sess-1", modelID, 1000, 250) // 0.25 tokens per byte
	hdr := http.Header{}
	hdr.Set("x-test-session", "sess-1")
	count, tier = countAnswer(t, countRequest(d, rk, body, hdr))
	if tier != "calibrated" || count != len(body)/4 {
		t.Fatalf("count = %d, tier = %q, want the session ratio applied", count, tier)
	}
}

// Nothing known: the answer is the client's own character heuristic, so it is never worse than the fallback it replaces.
func TestCountTokens_EstimatedTier(t *testing.T) {
	cat, rk := countCatalog(t, "http://upstream.invalid")
	d := buildRunnableDeps(t, cat)
	d.Profiles = profileRegistry(newCountProfile())

	body := `{"model":"test-model","messages":[]}`
	count, tier := countAnswer(t, countRequest(d, rk, body, nil))
	if tier != "estimated" {
		t.Fatalf("tier = %q, want estimated", tier)
	}
	if want := len(body) / 4; count != want {
		t.Fatalf("count = %d, want %d", count, want)
	}
}

// A model the key cannot route must fail exactly as it does on the messages endpoint — same status, same envelope.
func TestCountTokens_UnknownModel(t *testing.T) {
	cat, rk := countCatalog(t, "http://upstream.invalid")
	d := buildRunnableDeps(t, cat)
	d.Profiles = profileRegistry(newCountProfile())

	w := countRequest(d, rk, `{"model":"no-such-model","messages":[]}`, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body: %s", w.Code, w.Body.String())
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Code != "model_not_found" {
		t.Errorf("error code = %q, want model_not_found", e.Error.Code)
	}
}

// Mounted for real, the route coexists with the profile's messages route and runs the same auth chain as /v1/*.
func TestMount_CountTokensRouteNeedsARelayKey(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	d.Pinger = stubPinger{}
	d.Profiles = profileRegistry(newCountProfile())

	r := chi.NewRouter()
	Mount(r, d)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/counting/v1/messages/count_tokens",
		strings.NewReader(`{"model":"test-model"}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("count route answered without credentials: %s", rec.Body.String())
	}
	if rec.Code == http.StatusNotFound {
		t.Fatal("count route not reachable once mounted")
	}
}

// The route exists only for a profile that declares it.
func TestMountTokenCountRoutes_OnlyForDeclaringProfiles(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	d.Profiles = profileRegistry(newCountProfile(), stubProfile{})

	r := chi.NewRouter()
	mountTokenCountRoutes(r, d)
	paths := routePaths(t, r)

	if !contains(paths, "/counting/v1/messages/count_tokens") {
		t.Errorf("declaring profile has no route: %v", paths)
	}
	if contains(paths, "/stub/v1/messages/count_tokens") {
		t.Errorf("a profile that declares no count route must not get one: %v", paths)
	}
}
