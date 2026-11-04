package inference

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// newScriptStore returns a fresh store for one test with the limiter's and key pool's scripts runnable through a wrapper.
type newScriptStore func(t *testing.T) kvtest.ScriptStore

func memScriptStore(t *testing.T) kvtest.ScriptStore {
	m := kv.NewMem()
	t.Cleanup(func() { _ = m.Close() })
	pkgratelimit.RegisterScripts(m)
	keypool.RegisterScripts(m)
	return m
}

// A plain request, a user key reaching one credential in a provider account whose policy limits it, spends two store round trips before the upstream sees it: the pick's breaker read, with the caller hold when the caller's own policy limits it, then the account's admit.
func TestPlainRequestRoundTrips(t *testing.T) { runPlainRequestRoundTrips(t, memScriptStore) }

func runPlainRequestRoundTrips(t *testing.T, store newScriptStore) {
	t.Run("account_limit", func(t *testing.T) { plainRequestRoundTrips(t, store, false, 2) })
	t.Run("caller_and_account_limits", func(t *testing.T) { plainRequestRoundTrips(t, store, true, 2) })
}

func plainRequestRoundTrips(t *testing.T, newStore newScriptStore, callerLimited bool, want int) {
	rec := kvtest.NewRecording(newStore(t))
	var atCall atomic.Int64
	atCall.Store(-1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atCall.Store(int64(rec.RoundTrips()))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	var w plantest.World
	prov := w.Provider("openai")
	h := w.Host("openai", up.URL)
	w.Bind(w.Model(prov, "test-model"), h, adapters.OpenAI)
	acct := w.Account(h)
	w.Credential(h, acct, plantest.System, "sk-account")
	perMinute := w.RateLimit(ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: 1 << 20, Window: ratelimit.Window(time.Minute), Strategy: ratelimit.StrategyFixedWindow})
	limited := policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: perMinute.Meta.ID}}}
	w.BindPolicy(w.Policy("tier", limited), acct)
	u := w.User()
	k := w.UserKey(u, "plain-secret")
	if callerLimited {
		w.BindPolicy(w.Policy("caller", limited), w.PersonalAccount(u))
	}
	plans := w.Load(t)
	cat := catalogtest.Catalog{Providers: w.Providers, Hosts: w.Hosts, Models: w.Models, Bindings: w.Bindings, HostKeys: w.Credentials, Keys: w.Keys}.Load(t)
	kp := plans.Root().Keys.Lookup(k.Spec.KeyHash, time.Now())
	if kp == nil {
		t.Fatal("no key plan")
	}
	pr := principalOf(kp, k.Spec.KeyHash)
	if hop := pr.Caller.Routes.Get("test-model"); hop.Refused != nil {
		t.Fatalf("route refused: %+v", hop.Refused)
	}

	reg := buildTestRegistry()
	d := Deps{
		Catalog: cat, Plans: plans.Holder, Adapters: reg.AdapterMap(), Specs: reg,
		Pipeline: &pipeline.Pipeline{
			Limiter:  lua.New(pkgratelimit.New(rec, slog.New(slog.DiscardHandler), nil)),
			Selector: keypool.New(rec, slog.New(slog.DiscardHandler), nil, nil),
			Logger:   slog.New(slog.DiscardHandler),
		},
	}
	r := withNormalContext(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), pr)
	rw := httptest.NewRecorder()
	Dispatch(d, rw, r, DispatchInput{Inbound: adapters.OpenAI, Body: []byte(`{"model":"test-model"}`), ModelName: "test-model"})
	if rw.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rw.Code, rw.Body)
	}
	if n := atCall.Load(); n != int64(want) {
		t.Fatalf("store round trips before the upstream call = %d, want %d (batches %v)", n, want, rec.Batches())
	}
	reserves := 1
	if callerLimited {
		reserves++
	}
	if n := len(rec.Named("limit.reserve")); n != reserves {
		t.Fatalf("reserve scripts = %d, want %d: one per limited side", n, reserves)
	}
}

// An authed proxy request holds the caller's counters and the system ceiling, two atomicity groups, in one round trip before the upstream sees it.
func TestProxyRequestRoundTrips(t *testing.T) {
	var (
		rec    *kvtest.Recording
		atCall atomic.Int64
	)
	atCall.Store(-1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atCall.Store(int64(rec.RoundTrips()))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	ceiling := &ratelimit.RateLimit{
		Meta: meta.Metadata{ID: meta.NewID(), Name: systemRLProxyAuthed, Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 1 << 20, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow}}},
	}
	d, pr, store := proxyStreamDepsLimited(t, up.URL, 1<<40, ceiling)
	rec = store
	w := serveProxyShape(d, proxyShapeCall{principal: pr, pin: "anthropic", body: strings.NewReader(`{"model":"test-model","messages":[]}`), length: -1})
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if n := atCall.Load(); n != 1 {
		t.Fatalf("store round trips before the upstream call = %d, want 1 (batches %v)", n, rec.Batches())
	}
	if n := len(rec.Named("limit.reserve")); n != 2 {
		t.Fatalf("reserve scripts = %d, want 2: the caller's group and the ceiling's", n)
	}
}
