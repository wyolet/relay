package inference

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/google/go-cmp/cmp"
	"github.com/wyolet/relay/app/account"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/revocation"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/meta"
)

// listingWorld serves "allowed" and "denied" at a keyed host through a system credential, and "keyless" at a host that needs no key and no pool reaches; the user's policy grants "allowed" and "keyless".
func listingWorld(t *testing.T) (*plantest.Plans, *Principal) {
	t.Helper()
	var w plantest.World
	prov := w.Provider("prov")
	keyed := w.Host("keyed", "http://keyed.invalid")
	keyless := w.Host("keyless-host", "http://keyless.invalid")
	keyless.Spec.NoAuth = true
	for _, name := range []string{"allowed", "denied"} {
		w.Bind(w.Model(prov, name), keyed, adapters.OpenAI)
	}
	w.Bind(w.Model(prov, "keyless"), keyless, adapters.OpenAI)
	w.Credential(keyed, nil, plantest.System, "sk-keyed")
	u := w.User()
	k := w.UserKey(u, "listing-secret")
	pol := w.Policy("grants", policy.Spec{Models: []string{"prov/allowed", "prov/keyless"}})
	w.BindPolicy(pol, w.PersonalAccount(u))
	plans := w.Load(t)
	kp := plans.Root().Keys.Lookup(k.Spec.KeyHash, time.Now())
	if kp == nil {
		t.Fatal("no key plan")
	}
	return plans, principalOf(kp, k.Spec.KeyHash)
}

// A keyless host ignores the caller's own upstream key, so listing or pinning one in proxy mode needs a pool at it under a caller that is not refused, as normal routing does. A keyed host is open to anyone bringing a key.
func TestProxyHostsFor(t *testing.T) {
	_, p := listingWorld(t)
	snap := p.Caller.Routes.Catalog()
	pooled := *p.Caller
	for _, h := range snap.Hosts() {
		if h.Meta.Name == "keyless-host" {
			pooled.Pools = append(pooled.Pools, &plan.PoolPlan{Host: h})
		}
	}
	refused := pooled
	refused.Refused = &plan.Refusal{Code: plan.CodeAccountClosed}
	as := func(c *plan.CallerPlan) *Principal {
		cp := *p
		cp.Caller = c
		return &cp
	}
	for _, tc := range []struct {
		name string
		p    *Principal
		want []proxyHostEntry
	}{
		{"no pool at the keyless host", p, []proxyHostEntry{{Slug: "keyed"}}},
		{"pool at the keyless host", as(&pooled), []proxyHostEntry{{Slug: "keyed"}, {Slug: "keyless-host"}}},
		{"refused caller holding the pool", as(&refused), []proxyHostEntry{{Slug: "keyed"}}},
		{"no caller plan", as(nil), []proxyHostEntry{{Slug: "keyed"}}},
		{"anonymous", nil, []proxyHostEntry{{Slug: "keyed"}}},
	} {
		got := proxyHostsFor(snap, tc.p)
		sort.Slice(got, func(i, j int) bool { return got[i].Slug < got[j].Slug })
		if diff := cmp.Diff(tc.want, got); diff != "" {
			t.Errorf("%s (-want +got):\n%s", tc.name, diff)
		}
	}
}

// With proxy mode off, which is how it ships, the host list is refused.
func TestProxyHosts_RefusedWhileProxyModeIsOff(t *testing.T) {
	httpapi.InstallErrorRewriter()
	cat, pr := buildDispatchCatalog(t, "openai", adapters.OpenAI)
	_, api := humatest.New(t)
	registerProxyHosts(api, buildDeps(t, cat), nil)
	resp := api.GetCtx(withNormalContext(httptest.NewRequest(http.MethodGet, "/", nil), pr).Context(), "/v1/proxy/hosts")
	if env := decodeEnvelope(t, resp.Body.Bytes()); resp.Code != http.StatusForbidden || env.Message != "proxy mode is not enabled" {
		t.Fatalf("status %d, envelope %+v, want a 403 naming proxy mode", resp.Code, env)
	}
}

// Authentication reads only the published plans and the in-memory revoked set: no bearer, rejected or admitted, reaches the store.
func TestRejectedCredentialsNeverTouchTheStore(t *testing.T) {
	f := newPrincipalFixture()
	st := f.stack(t, saKey(f, "sk-wr-live"))
	mem := kv.NewMem()
	t.Cleanup(func() { _ = mem.Close() })
	rec := kvtest.NewRecording(mem)
	// The revoked set is the one store-backed thing authentication holds; it must answer from memory.
	auth := &Auth{Catalog: st.cat, Plans: st.plans.Holder, Revoked: revocation.New(rec).Set(), Tokens: f.tokens}
	h := ClassifyMiddleware()(PrincipalMiddleware(auth)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	for _, bearer := range []string{"sk-wr-unknown", "ey.not.a-token", f.mint(t, func(c *crypto.TokenClaims) { c.Ver = 99 }), "sk-wr-live", f.mint(t, nil)} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer "+bearer)
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if n := rec.RoundTrips(); n != 0 {
		t.Fatalf("store round trips = %d, want 0", n)
	}
}

// A WebSocket connection re-authenticates each frame against the current plans: a key revoked, or a token signed out everywhere, after the upgrade stops the next frame.
func TestFrameReauthenticates(t *testing.T) {
	f := newPrincipalFixture()
	k := saKey(f, "sk-wr-live")
	st := f.stack(t, k)
	if w := st.do("sk-wr-live"); w.Code != http.StatusOK {
		t.Fatalf("upgrade status %d", w.Code)
	}
	upgraded := st.seen
	if _, ok := st.auth.reauthenticate(httptest.NewRecorder(), nil, upgraded); !ok {
		t.Fatal("a live key must pass a frame")
	}
	past := time.Now().Add(-time.Minute)
	revoked := *k
	revoked.Spec.RevokedAt = &past
	plantest.Apply(t, st.plans, &revoked)
	w := httptest.NewRecorder()
	if _, ok := st.auth.reauthenticate(w, nil, upgraded); ok || w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: ok=%v status %d, want a 401", ok, w.Code)
	}

	token := f.mint(t, nil)
	if w := st.do(token); w.Code != http.StatusOK {
		t.Fatalf("token upgrade status %d", w.Code)
	}
	tokenPrincipal := st.seen
	plantest.Apply(t, st.plans, &user.User{ID: f.user, Username: "u-" + f.user, TokenVersion: 2})
	w = httptest.NewRecorder()
	if _, ok := st.auth.reauthenticate(w, nil, tokenPrincipal); ok || w.Code != http.StatusUnauthorized {
		t.Fatalf("signed-out token: ok=%v status %d, want a 401", ok, w.Code)
	}
}

// Bringing an upstream key does not lift the operator's proxy ceiling: an authed proxy request holds the system bucket per credential, beside the caller's own counters, and an anonymous one holds the anonymous bucket per client IP.
func TestProxy_SystemCeiling(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	for _, tc := range []struct {
		name, bucket string
		authed       bool
	}{
		{"authed", systemRLProxyAuthed, true},
		{"anonymous", systemRLProxyAnon, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ceiling := &ratelimit.RateLimit{
				Meta: meta.Metadata{ID: meta.NewID(), Name: tc.bucket, Owner: meta.Owner{Kind: meta.OwnerSystem}},
				Spec: ratelimit.Spec{Rules: []ratelimit.Rule{{Meter: ratelimit.MeterRequests, Amount: 1, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow}}},
			}
			d, pr, _ := proxyStreamDepsLimited(t, up.URL, 1<<40, ceiling)
			call := proxyShapeCall{pin: "anthropic", length: -1}
			if tc.authed {
				call.principal = pr
			}
			send := func() *httptest.ResponseRecorder {
				call.body = strings.NewReader(`{"model":"test-model","messages":[]}`)
				return serveProxyShape(d, call)
			}
			if w := send(); w.Code != http.StatusOK {
				t.Fatalf("first request = %d %s", w.Code, w.Body)
			}
			w := send()
			if w.Code != http.StatusTooManyRequests || envelopeOf(t, w).Code != "rate_limit_exceeded" {
				t.Fatalf("second request = %d %s, want the ceiling's 429", w.Code, w.Body)
			}
		})
	}
}

// Payload capture: the nearest account on the lane's ladder that sets payloadLogging decides, else the key's own flag.
func TestCapturesPayload(t *testing.T) {
	for _, tc := range []struct {
		name        string
		lane        account.PayloadLogging
		keyCaptures bool
		want        bool
	}{
		{"account on, key off", account.PayloadLoggingOn, false, true},
		{"account off, key on", account.PayloadLoggingOff, true, false},
		{"unset, key on", "", true, true},
		{"unset, key off", "", false, false},
	} {
		cp := &plan.CallerPlan{PayloadLogging: tc.lane}
		p := &Principal{Caller: cp, PayloadLogging: tc.keyCaptures}
		if got := p.CapturesPayload(); got != tc.want {
			t.Errorf("%s: captures = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A key of a disabled user no longer authenticates, by the caller ladder's refusal.
func TestDisabledUserKeyIsRefused(t *testing.T) {
	f := newPrincipalFixture()
	k := &key.Key{Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalUser, ID: f.user}, KeyHash: sha("sk-wr-personal")}}
	k.Meta.ID, k.Meta.Name = "k-personal", "personal"
	st := f.stack(t, k)
	plantest.Apply(t, st.plans, &user.User{ID: f.user, Username: "u-" + f.user, TokenVersion: 1, Disabled: true})
	wantUnauthenticated(t, st.do("sk-wr-personal"), "api key disabled")
}
