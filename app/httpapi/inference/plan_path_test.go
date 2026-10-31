package inference

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/revocation"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
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
	w.BindPolicy(pol, plantest.Subject(rolebinding.SubjectUser, u.ID))
	plans := w.Load(t)
	kp := plans.Root().Keys.Lookup(k.Spec.KeyHash, time.Now())
	if kp == nil {
		t.Fatal("no key plan")
	}
	return plans, principalOf(kp, k.Spec.KeyHash)
}

// /v1/models never advertises a model a request would be refused: a model outside the grant, and one granted only at a keyless host no pool reaches, are both refused by the route and both absent from the list.
func TestListingMatchesRouting(t *testing.T) {
	_, p := listingWorld(t)
	ctx := context.WithValue(context.Background(), ctxPrincipalT{}, p)
	out, err := listModels(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, row := range out.Body.Data {
		listed = append(listed, row.ID)
	}
	sort.Strings(listed)
	if len(listed) != 1 || listed[0] != "allowed" {
		t.Fatalf("listed = %v, want [allowed]", listed)
	}
	for _, name := range []string{"allowed", "denied", "keyless"} {
		r := p.Caller.Routes.Get(name)
		if routed := r.Refused == nil; routed != (name == "allowed") {
			t.Errorf("route %s refused = %v, listing says listed = %v", name, r.Refused, name == "allowed")
		}
	}
}

// A keyless host ignores the caller's own upstream key, so pinning one in proxy mode needs a pool at it, as normal routing does.
func TestProxyPinKeylessHostNeedsAPool(t *testing.T) {
	_, p := listingWorld(t)
	keyless, keyed := findHost(t, p, "keyless-host"), findHost(t, p, "keyed")
	if proxyPinAllowed(p, keyless) {
		t.Fatal("a keyless host with no pool was pinnable")
	}
	if !proxyPinAllowed(p, keyed) {
		t.Fatal("a keyed host must be pinnable with the caller's own key")
	}
	cp := *p.Caller
	cp.Pools = append(cp.Pools, &plan.PoolPlan{Host: keyless})
	withPool := *p
	withPool.Caller = &cp
	if !proxyPinAllowed(&withPool, keyless) {
		t.Fatal("a pool at the keyless host is the grant that makes it pinnable")
	}
}

func findHost(t *testing.T, p *Principal, name string) *host.Host {
	t.Helper()
	for _, h := range p.Caller.Routes.Catalog().Hosts() {
		if h.Meta.Name == name {
			return h
		}
	}
	t.Fatalf("no host %s", name)
	return nil
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
	if _, ok := st.auth.reauthenticate(httptest.NewRecorder(), upgraded); !ok {
		t.Fatal("a live key must pass a frame")
	}
	past := time.Now().Add(-time.Minute)
	revoked := *k
	revoked.Spec.RevokedAt = &past
	plantest.Apply(t, st.plans, &revoked)
	w := httptest.NewRecorder()
	if _, ok := st.auth.reauthenticate(w, upgraded); ok || w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked key: ok=%v status %d, want a 401", ok, w.Code)
	}

	token := f.mint(t, nil)
	if w := st.do(token); w.Code != http.StatusOK {
		t.Fatalf("token upgrade status %d", w.Code)
	}
	tokenPrincipal := st.seen
	plantest.Apply(t, st.plans, &user.User{ID: f.user, Username: "u-" + f.user, TokenVersion: 2})
	w = httptest.NewRecorder()
	if _, ok := st.auth.reauthenticate(w, tokenPrincipal); ok || w.Code != http.StatusUnauthorized {
		t.Fatalf("signed-out token: ok=%v status %d, want a 401", ok, w.Code)
	}
}

// An upstream token count spends a credential, so it holds the caller's counters like a generation: the second count under a one-request budget is refused before the upstream.
func TestCountTokens_UpstreamCountIsMetered(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"input_tokens":7}`))
	}))
	defer up.Close()
	cat, rk := countCatalog(t, up.URL)
	limitCaller(t, cat, 1)
	d := buildRunnableDeps(t, cat)
	useSpecs(&d, countingRegistry())
	d.Profiles = profileRegistry(newCountProfile())

	body := `{"model":"test-model","messages":[]}`
	if count, tier := countAnswer(t, countRequest(d, rk, body, nil)); count != 7 || tier != "exact" {
		t.Fatalf("first count = %d/%s, want 7/exact", count, tier)
	}
	w := countRequest(d, rk, body, nil)
	if w.Code != http.StatusTooManyRequests || errCode(t, w.Body.Bytes()) != "rate_limit_exceeded" {
		t.Fatalf("second count = %d %s, want 429 rate_limit_exceeded", w.Code, w.Body)
	}
	if hits != 1 {
		t.Fatalf("upstream counted %d times, want 1", hits)
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
			if w.Code != http.StatusTooManyRequests || errCode(t, w.Body.Bytes()) != "rate_limit_exceeded" {
				t.Fatalf("second request = %d %s, want the ceiling's 429", w.Code, w.Body)
			}
		})
	}
}

// Payload capture: the innermost caller policy decides when one governs, else the key's own flag.
func TestCapturesPayload(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name           string
		policyCaptures *bool
		keyCaptures    bool
		want           bool
	}{
		{"policy on, key off", &on, false, true},
		{"policy off, key on", &off, true, false},
		{"no policy, key on", nil, true, true},
		{"no policy, key off", nil, false, false},
	} {
		cp := &plan.CallerPlan{}
		if tc.policyCaptures != nil {
			cp.Rungs[1].Policy = &plan.ExpandedPolicy{Policy: &policy.Policy{Spec: policy.Spec{PayloadLoggingEnabled: *tc.policyCaptures}}}
		}
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
	w := st.do("sk-wr-personal")
	if w.Code != http.StatusUnauthorized || authCode(t, w.Body.Bytes()) != "api key disabled" {
		t.Fatalf("status %d %s, want 401 api key disabled", w.Code, w.Body)
	}
}
