package inference

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	"github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
)

// proxyDupCatalog holds the anthropic-adapter model "test-model" on the no-auth host "anthropic" at upstreamURL, a policy granting it, a passthrough key under that policy, and the proxy-authed system rate limit.
func proxyDupCatalog(t *testing.T, upstreamURL string) (*catalog.Catalog, *Principal) {
	t.Helper()
	provID, hostID, hkID, modID, polID := meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID(), meta.NewID()
	systemRL := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: systemRLProxyAuthed, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 100, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow,
		}}},
	}
	k := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "rk", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: key.Spec{PolicyID: polID, KeyHash: "proxyhash", PassthroughAllowed: true},
	}
	cat := catalogtest.Catalog{
		Providers: []*provider.Provider{{Meta: meta.Metadata{ID: provID, Name: "anthropic", Owner: meta.Owner{Kind: meta.OwnerSystem}}}},
		Hosts: []*host.Host{{
			Meta: meta.Metadata{ID: hostID, Name: "anthropic", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: host.Spec{BaseURL: upstreamURL, NoAuth: true},
		}},
		HostKeys: []*hostkey.HostKey{{
			Meta: meta.Metadata{ID: hkID, Name: "k", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
			Spec: hostkey.Spec{HostID: hostID, PolicyID: polID, Value: "sk-dummy", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
		}},
		Models: []*model.Model{{
			Meta: meta.Metadata{ID: modID, Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: provID}},
			Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From("test-model")}}, Pointer: slug.From("test-model")},
		}},
		Bindings: []*binding.Binding{{
			Meta: meta.Metadata{ID: meta.NewID(), Name: "test-model-binding", Owner: meta.Owner{Kind: meta.OwnerSystem}},
			Spec: binding.Spec{ModelID: modID, HostID: hostID, Adapter: adapters.Anthropic},
		}},
		Policies: []*policy.Policy{{
			Meta: meta.Metadata{ID: polID, Name: "p", Owner: meta.Owner{Kind: meta.OwnerHost, ID: hostID}},
			Spec: policy.Spec{ModelIDs: []string{modID}, HostKeyIDs: []string{hkID}},
		}},
		RateLimits: []*ratelimit.RateLimit{systemRL},
		Keys:       []*key.Key{k},
	}.Load(t)
	pol, ok := cat.Current().Policy(polID)
	if !ok {
		t.Fatal("policy missing from snapshot")
	}
	return cat, &Principal{CredentialKind: CredentialKey, CredentialID: k.Meta.ID, KeyHash: k.Spec.KeyHash, Key: k, Policy: pol, PassthroughAllowed: true}
}

// proxyDupDeps wires deps whose proxy runner reserves through a limiter over a recording kv.Mem, so a test can read how a reservation settled.
func proxyDupDeps(t *testing.T, cat *catalog.Catalog) (Deps, *kvtest.Recording) {
	t.Helper()
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	t.Cleanup(func() { _ = mem.Close() })
	store := kvtest.NewRecording(mem)
	d := buildDeps(t, cat)
	d.Proxy = proxy.New(pkgratelimit.New(store, slog.New(slog.DiscardHandler), nil), nil, nil)
	return d, store
}

const duplicateModelBody = `{"model":"test-model","messages":[{"role":"user","content":"hi"}],"model":"other-model"}`

func assertDuplicateModel(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400; body %s", w.Code, w.Body)
	}
	if e := parseDispatchErr(t, w.Body.Bytes()); e.Error.Type != "invalid_request_error" || e.Error.Code != "duplicate_model" {
		t.Fatalf("envelope %s/%s, want invalid_request_error/duplicate_model", e.Error.Type, e.Error.Code)
	}
}

// Normal mode routes on encoding/json's last model while a byte-pass may forward both, so the body is refused before routing.
func TestHandleShape_NormalMode_DuplicateModel(t *testing.T) {
	cat, prin := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(duplicateModelBody)), prin)
	w := httptest.NewRecorder()
	handleShape(d.Specs.Spec(adapters.OpenAI), d, w, r)
	assertDuplicateModel(t, w)
}

func TestHandleCountTokens_DuplicateModel(t *testing.T) {
	cat, prin := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages/count_tokens", strings.NewReader(duplicateModelBody)), prin)
	w := httptest.NewRecorder()
	handleCountTokens(d, w, r)
	assertDuplicateModel(t, w)
}

func TestHandleShape_ProxyMode_DuplicateModel(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer up.Close()
	cat, _ := proxyDupCatalog(t, up.URL)
	d, _ := proxyDupDeps(t, cat)
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(duplicateModelBody))
	r.Header.Set(httpheader.HeaderProxyMode, httpheader.ProxyModeValueProxy)
	r.Header.Set(httpheader.HeaderRelayAPIKey, "relay-key")
	r.Header.Set("Authorization", "Bearer upstream-key")
	w := httptest.NewRecorder()
	ClassifyMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleShape(d.Specs.Spec(adapters.Anthropic), d, w, r)
	})).ServeHTTP(w, r)
	assertDuplicateModel(t, w)
	if hits != 0 {
		t.Fatalf("upstream called %d times", hits)
	}
}

// Proxy mode forwards the body as sent, so a model key a case-sensitive upstream would not read is refused; normal mode rewrites the key and routes it.
func TestHandleShape_ModelKeySpelling(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	d := buildDeps(t, cat)
	for _, body := range []string{`{"Model":"test-model","messages":[]}`, `{"` + uEscape("006d") + `odel":"test-model","messages":[]}`} {
		r := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(body))
		r = r.WithContext(WithClassification(r.Context(), Classification{Mode: ModeProxyAnonymous, UpstreamAuth: "Bearer k", UpstreamHost: "openai"}))
		w := httptest.NewRecorder()
		handleShape(d.Specs.Spec(adapters.OpenAI), d, w, r)
		if w.Code != http.StatusBadRequest || parseDispatchErr(t, w.Body.Bytes()).Error.Code != "invalid_model_key" {
			t.Fatalf("proxy mode, %s: status %d %s, want 400 invalid_model_key", body, w.Code, w.Body)
		}

		if m, _, err := extractModelStream([]byte(body)); err != nil || m != "test-model" {
			t.Fatalf("normal mode, %s: (%q, %v), want the model", body, m, err)
		}
	}
}

// Past the peek window the body streams upstream while it is checked: a second model there aborts the forward with the body incomplete, refunds the reservation and records the refusal, not a success. handleProxy's proxy-mode gate reads a settings store a unit test has none of, so this drives the steps it runs after the gate.
func TestProxy_DuplicateModelPastThePeekWindow(t *testing.T) {
	type upstreamSaw struct {
		complete bool
		n        int
	}
	saw := make(chan upstreamSaw, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		saw <- upstreamSaw{complete: err == nil && json.Valid(b), n: len(b)}
		_, _ = w.Write([]byte(`{"id":"m","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()

	cat, pr := proxyDupCatalog(t, up.URL)
	d, store := proxyDupDeps(t, cat)
	kinds := make(chan string, 4)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "kind", Fn: func(_ *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		kinds <- ev.ErrorKind
		return nil, nil
	}})
	d.Proxy.Lifecycle = reg

	send := func(body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(body))
		ctx := withPrincipal(r.Context(), d.Catalog.Current(), pr, pr.Key)
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		plan, pb, err := resolveProxyHostByPolicy(r, d.Resolver, pr, false)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		res, err := d.Proxy.Run(ctx, &proxy.Request{
			Method:       http.MethodPost,
			Path:         "/v1/messages",
			Body:         pb.Reader,
			HostBaseURL:  plan.Host.Spec.BaseURL,
			UpstreamAuth: "Bearer upstream-key",
			RateScope:    pr.KeyHash,
			Rules:        resolveSystemRules(d.Catalog.Current(), systemRLProxyAuthed, pr.KeyHash),
			Lifecycle:    lifecycle.NewContext("req-dup", "proxy", time.Now()),
		})
		if err != nil {
			mapProxyErr(w, err)
			return w
		}
		defer res.Body.Close()
		if rej := pb.Scanned.rejection(); rej != nil {
			writeAPIError(w, rej.status, "invalid_request_error", rej.code, rej.msg)
			return w
		}
		w.WriteHeader(res.Status)
		_, _ = io.Copy(w, res.Body)
		return w
	}

	pad := strings.Repeat("a", 2*proxyMaxBodyPeek)
	w := send([]byte(`{"model":"test-model","messages":[{"role":"user","content":"` + pad + `"}],"model":"other-model"}`))
	assertDuplicateModel(t, w)
	select {
	case s := <-saw:
		if s.complete {
			t.Fatalf("upstream received a complete body (%d bytes)", s.n)
		}
	case <-time.After(5 * time.Second):
	}
	select {
	case k := <-kinds:
		if k != "duplicate_model" {
			t.Fatalf("recorded as %q, want duplicate_model", k)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no usage event for the refusal")
	}

	// Settled as cancelled (commit ARGV[7]), so no tokens are booked against the limit.
	commits := store.Named("limit.commit")
	if len(commits) != 1 || len(commits[0].Args) < 7 || commits[0].Args[6] != int64(1) {
		t.Fatalf("reservation settlement: %+v, want one cancelled commit", commits)
	}

	if w := send([]byte(`{"model":"test-model","messages":[{"role":"user","content":"` + pad + `"}]}`)); w.Code != http.StatusOK {
		t.Fatalf("clean request after the refusal: status %d body %s", w.Code, w.Body)
	}
	if s := <-saw; !s.complete {
		t.Fatalf("clean body arrived incomplete (%d bytes)", s.n)
	}
}
