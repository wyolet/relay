package inference

// hotpath_guard_test.go pins what a whole /v1/* request may cost — no
// catalog-store read, one kv script — measured through the handler rather
// than at policy.Service, because the budget is a property of the request.

import (
	"context"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/group"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/policybinding"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/project"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/role"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/app/routing"
	"github.com/wyolet/relay/app/serviceaccount"
	"github.com/wyolet/relay/app/team"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
	"github.com/wyolet/relay/pkg/slug"
)

// --- kv accounting -------------------------------------------------------

// countingStore counts every kv.Store method except Close, so totalOps()==0
// means the request made no kv contact of any kind rather than "none of the
// few methods this file happened to wrap". RunScriptBatch counts per call it
// carries: the commit path collapses two into one batch, and an uncounted
// batch would read as a free round trip.
type countingStore struct {
	*kv.Mem

	mu      sync.Mutex
	scripts []string
	ops     int
}

func newCountingStore(t testing.TB) *countingStore {
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	t.Cleanup(func() { _ = mem.Close() })
	return &countingStore{Mem: mem}
}

func (c *countingStore) record(name string) {
	c.mu.Lock()
	if name != "" {
		c.scripts = append(c.scripts, name)
	}
	c.ops++
	c.mu.Unlock()
}

func (c *countingStore) RunScript(ctx context.Context, name, script string, keys []string, args ...any) ([]byte, error) {
	c.record(name)
	return c.Mem.RunScript(ctx, name, script, keys, args...)
}

func (c *countingStore) RunScriptBatch(ctx context.Context, calls []kv.ScriptCall) []kv.ScriptResult {
	for _, call := range calls {
		c.record(call.Name)
	}
	return c.Mem.RunScriptBatch(ctx, calls)
}

func (c *countingStore) Get(ctx context.Context, k string) ([]byte, error) {
	c.record("")
	return c.Mem.Get(ctx, k)
}

func (c *countingStore) Set(ctx context.Context, k string, v []byte, ttl time.Duration) error {
	c.record("")
	return c.Mem.Set(ctx, k, v, ttl)
}

func (c *countingStore) Del(ctx context.Context, k string) error {
	c.record("")
	return c.Mem.Del(ctx, k)
}

func (c *countingStore) Incr(ctx context.Context, k string, delta int64) (int64, error) {
	c.record("")
	return c.Mem.Incr(ctx, k, delta)
}

func (c *countingStore) Expire(ctx context.Context, k string, ttl time.Duration) error {
	c.record("")
	return c.Mem.Expire(ctx, k, ttl)
}

func (c *countingStore) Range(ctx context.Context, prefix string) ([]kv.Entry, error) {
	c.record("")
	return c.Mem.Range(ctx, prefix)
}

func (c *countingStore) WithLock(ctx context.Context, keys []string, fn func(context.Context) error) error {
	c.record("")
	return c.Mem.WithLock(ctx, keys, fn)
}

// meteringScripts counts the rate-limit calls, which is what the one-script
// budget is about; the key-pool breaker writes under its own name and runs
// post-flight, off the latency path.
func (c *countingStore) meteringScripts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, name := range c.scripts {
		if strings.HasPrefix(name, "limit.") {
			n++
		}
	}
	return n
}

// totalOps is every counted kv method call, script or not.
func (c *countingStore) totalOps() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ops
}

func (c *countingStore) scriptNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.scripts...)
}

// --- catalog stores that must never be read on the request path ----------

// armedList is a catalog store that panics once armed. Every app/X.Store is
// its package's Postgres boundary and the inference Deps holds no pool of
// its own, so arming these after the boot Reload is the sentinel for "the
// request path went to the database".
type armedList[T any] struct {
	rows  []*T
	armed *atomic.Bool
}

func (l armedList[T]) List(context.Context) ([]*T, error) {
	if l.armed.Load() {
		panic("catalog store read on the request path — /v1/* must serve from the snapshot")
	}
	return l.rows, nil
}

type armedVersions struct {
	rows  map[string]int
	armed *atomic.Bool
}

func (v armedVersions) TokenVersions(context.Context) (map[string]int, error) {
	if v.armed.Load() {
		panic("token-version store read on the request path")
	}
	return v.rows, nil
}

// --- the fixture ---------------------------------------------------------

// hotPathFixture is a complete data plane: tenancy, a routable model on a
// live upstream, and the kv store every metering call goes through.
type hotPathFixture struct {
	cat     *appcatalog.Catalog
	deps    Deps
	store   *countingStore
	tokens  *TokenVerifier
	handler http.Handler

	team    *team.Team
	project *project.Project
	sa      *serviceaccount.ServiceAccount
	keyRow  *key.Key
	user    string

	// scriptsAtUpstreamCall is the metering-script count observed when the
	// upstream was called: everything the request path itself paid, with
	// post-flight not yet started.
	scriptsAtUpstreamCall atomic.Int64
	upstreamCalls         atomic.Int64

	postFlight chan struct{}
	armed      *atomic.Bool
	mint       func(testing.TB, func(*crypto.TokenClaims)) string
}

// newHotPathFixture wires the catalog, the policy service and a live
// upstream. metered says whether the inbound policy carries a rate-limit
// rule; without one a key request has nothing to reserve.
func newHotPathFixture(t testing.TB, metered bool) *hotPathFixture {
	t.Helper()

	fx := &hotPathFixture{postFlight: make(chan struct{}, 8), armed: &atomic.Bool{}}
	fx.store = newCountingStore(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fx.scriptsAtUpstreamCall.Store(int64(fx.store.meteringScripts()))
		fx.upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"chat.completion",` +
			`"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`))
	}))
	t.Cleanup(upstream.Close)

	tm := &team.Team{Meta: meta.Metadata{ID: meta.NewID(), Name: "platform", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	proj := &project.Project{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "ml-search"},
		Spec: project.Spec{TeamID: tm.Meta.ID},
	}
	proj.StampOwner()
	sa := &serviceaccount.ServiceAccount{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer"},
		Spec: serviceaccount.Spec{ProjectID: proj.Meta.ID},
	}
	sa.StampOwner()
	user := meta.NewID()

	prov := &provider.Provider{Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
	h := &host.Host{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: host.Spec{BaseURL: upstream.URL},
	}
	// The host key's tier policy caps nothing: an upstream rate-limit rule
	// would be a second, legitimate script and would mask a regression in the
	// inbound one. Its empty spec is the implicit wildcard the tier gate wants.
	tierPol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream-tier", Owner: meta.Owner{Kind: meta.OwnerHost, ID: h.Meta.ID}},
	}
	hk := &hostkey.HostKey{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "upstream-key", Owner: meta.Owner{Kind: meta.OwnerHost, ID: h.Meta.ID}},
		Spec: hostkey.Spec{HostID: h.Meta.ID, PolicyID: tierPol.Meta.ID, Value: "sk-upstream", ValueFrom: hostkey.ValueFrom{Kind: hostkey.ValueKindStored}},
	}
	m := &model.Model{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "test-model", Owner: meta.Owner{Kind: meta.OwnerProvider, ID: prov.Meta.ID}},
		Spec: model.Spec{Snapshots: []model.Snapshot{{Name: slug.From("test-model")}}, Pointer: slug.From("test-model")},
	}
	b := &binding.Binding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "test-model-upstream", Owner: meta.Owner{Kind: meta.OwnerSystem}},
		Spec: binding.Spec{ModelID: m.Meta.ID, HostID: h.Meta.ID, Adapter: adapters.OpenAI},
	}

	var rls []*ratelimit.RateLimit
	pol := &policy.Policy{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "project-policy", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}},
		Spec: policy.Spec{ModelIDs: []string{m.Meta.ID}, HostKeyIDs: []string{hk.Meta.ID}},
	}
	if metered {
		rl := &ratelimit.RateLimit{Meta: meta.Metadata{ID: meta.NewID(), Name: "project-rl", Owner: meta.Owner{Kind: meta.OwnerSystem}}}
		rl.Spec.Rules = []ratelimit.Rule{{
			Meter: ratelimit.MeterRequests, Amount: 1_000_000, Window: ratelimit.Window(time.Hour),
		}}
		rls = append(rls, rl)
		pol.Spec.RateLimitID = rl.Meta.ID
	}
	sa.Spec.PolicyID = pol.Meta.ID

	keyRow := &key.Key{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "indexer-prod", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}},
		Spec: key.Spec{Principal: key.Principal{Kind: key.PrincipalServiceAccount, ID: sa.Meta.ID}, KeyHash: sha("sk-wr-live")},
	}
	// A token has no key row to hang a policy on, so a binding is its only
	// route to one.
	pb := &policybinding.PolicyBinding{
		Meta: meta.Metadata{ID: meta.NewID(), Name: "everyone", Owner: meta.Owner{Kind: meta.OwnerProject, ID: proj.Meta.ID}},
		Spec: policybinding.Spec{
			ProjectID: proj.Meta.ID, PolicyID: pol.Meta.ID,
			Subjects: []rolebinding.Subject{{Kind: rolebinding.SubjectGroup, Name: "system:authenticated"}},
		},
	}

	armed := fx.armed
	cat := appcatalog.New(
		armedList[provider.Provider]{rows: []*provider.Provider{prov}, armed: armed},
		armedList[host.Host]{rows: []*host.Host{h}, armed: armed},
		armedList[policy.Policy]{rows: []*policy.Policy{pol, tierPol}, armed: armed},
		armedList[model.Model]{rows: []*model.Model{m}, armed: armed},
		armedList[hostkey.HostKey]{rows: []*hostkey.HostKey{hk}, armed: armed},
		armedList[ratelimit.RateLimit]{rows: rls, armed: armed},
		armedList[key.Key]{rows: []*key.Key{keyRow}, armed: armed},
		armedList[pricing.Pricing]{armed: armed},
		armedList[binding.Binding]{rows: []*binding.Binding{b}, armed: armed},
	)
	cat.UseTenancy(
		armedList[team.Team]{rows: []*team.Team{tm}, armed: armed},
		armedList[project.Project]{rows: []*project.Project{proj}, armed: armed},
		armedList[serviceaccount.ServiceAccount]{rows: []*serviceaccount.ServiceAccount{sa}, armed: armed},
		armedList[group.Group]{armed: armed},
		armedList[role.Role]{armed: armed},
		armedList[rolebinding.RoleBinding]{armed: armed},
		armedList[policybinding.PolicyBinding]{rows: []*policybinding.PolicyBinding{pb}, armed: armed},
	)
	cat.UseTokenVersions(armedVersions{rows: map[string]int{user: 1}, armed: armed})
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatalf("catalog reload: %v", err)
	}

	fx.cat, fx.team, fx.project, fx.sa, fx.user, fx.keyRow = cat, tm, proj, sa, user, keyRow
	fx.tokens, fx.mint = hotPathTokens(t, proj.Meta.ID, user)

	svc := policy.NewService(
		hotPathSnapReader{cat: cat},
		keypool.New(fx.store, quietLogger(), nil, nil),
		pkgratelimit.New(fx.store, quietLogger(), nil),
	)

	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "hotpath-guard", Fn: func(*lifecycle.Context, *lifecycle.PostFlightEvent) (any, error) {
		select {
		case fx.postFlight <- struct{}{}:
		default:
		}
		return nil, nil
	}})

	fx.deps = Deps{
		Catalog:   cat,
		Tokens:    fx.tokens,
		Resolver:  routing.New(cat),
		Pipeline:  &pipeline.Pipeline{Policy: svc, Lifecycle: reg},
		Lifecycle: reg,
		Specs:     hotPathRegistry(),
	}
	fx.handler = ClassifyMiddleware()(PrincipalMiddleware(cat, fx.tokens)(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := []byte(`{"model":"test-model"}`)
			Dispatch(fx.deps, w, r, DispatchInput{Inbound: adapters.OpenAI, Body: body, ModelName: "test-model"})
		})))
	return fx
}

// hotPathRegistry registers the one inbound shape these requests speak; the
// upstream binding speaks it too, so dispatch byte-passes and no translator
// runs between the caller and the provider.
func hotPathRegistry() *adapter.Registry {
	spec := (&adapter.Spec{
		Name:        adapters.OpenAI,
		DefaultPath: "/v1/chat/completions",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
		Translator:  stubV1Translator{},
	}).Build()
	return adapter.NewRegistry(spec)
}

// hotPathSnapReader is the policy service's view of the catalog, serving the
// snapshot the request authenticated against — the adapter shape the
// composition root uses.
type hotPathSnapReader struct{ cat *appcatalog.Catalog }

func (r hotPathSnapReader) snap(ctx context.Context) *appcatalog.Snapshot {
	if s := SnapshotFrom(ctx); s != nil {
		return s
	}
	return r.cat.Current()
}

func (r hotPathSnapReader) Policy(ctx context.Context, id string) (*policy.Policy, bool) {
	return r.snap(ctx).Policy(id)
}

func (r hotPathSnapReader) RateLimit(ctx context.Context, id string) (*ratelimit.RateLimit, bool) {
	return r.snap(ctx).RateLimit(id)
}

// arm closes the door on the catalog stores: from here a store read panics,
// so any Postgres contact on the request path fails the test loudly.
func (f *hotPathFixture) arm() { f.armed.Store(true) }

func (f *hotPathFixture) do(t testing.TB, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
	r.Header.Set("Authorization", "Bearer "+bearer)
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

// waitPostFlight blocks until the detached post-flight goroutine is done.
// Its last kv call is the key pool's success record, which lands after the
// rate-limit commit — so once that is visible the metering count is final.
func (f *hotPathFixture) waitPostFlight(t testing.TB) {
	t.Helper()
	select {
	case <-f.postFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight never fired")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, name := range f.store.scriptNames() {
			if strings.HasPrefix(name, "keypool.") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("post-flight never reached its last kv call: %v", f.store.scriptNames())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// requestPathScripts is the metering budget spent by the time the upstream
// was called — everything after that point is post-flight.
func (f *hotPathFixture) requestPathScripts(t testing.TB) int {
	t.Helper()
	if f.upstreamCalls.Load() == 0 {
		t.Fatal("upstream was never called; the request did not reach the pipeline")
	}
	return int(f.scriptsAtUpstreamCall.Load())
}

// rebuildKey applies an admin-style edit to the key row and rebuilds the
// snapshot, then leaves the stores armed again for the request itself.
func (f *hotPathFixture) rebuildKey(t testing.TB, mutate func(*key.Key)) {
	t.Helper()
	f.armed.Store(false)
	mutate(f.keyRow)
	if err := f.cat.Reload(context.Background()); err != nil {
		t.Fatalf("reload after the key edit: %v", err)
	}
}

// hotPathTokens returns a verifier plus a minting function for tokens scoped
// to the fixture's project and user.
func hotPathTokens(t testing.TB, projectID, userID string) (*TokenVerifier, func(testing.TB, func(*crypto.TokenClaims)) string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	v := &TokenVerifier{}
	v.SetKey(pub)
	return v, func(t testing.TB, mutate func(*crypto.TokenClaims)) string {
		t.Helper()
		claims := crypto.TokenClaims{
			Iss: crypto.TokenIssuer,
			Sub: "user:" + userID,
			Prj: projectID,
			Ver: 1,
			Jti: meta.NewID(),
			Iat: time.Now().Unix(),
			Exp: time.Now().Add(time.Hour).Unix(),
		}
		if mutate != nil {
			mutate(&claims)
		}
		tok, err := crypto.SignToken(priv, "", claims)
		if err != nil {
			t.Fatalf("sign token: %v", err)
		}
		return tok
	}
}

// quietLogger keeps the limiter and the key pool from writing to the test
// output; both dereference their logger unconditionally.
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- the guards ----------------------------------------------------------

// TestKeyRequestCostsOneScript pins the metering budget for a key: one
// Reserve on the way in, one Commit in post-flight, and no store read at all.
func TestKeyRequestCostsOneScript(t *testing.T) {
	f := newHotPathFixture(t, true)
	f.arm()

	w := f.do(t, "sk-wr-live")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if got := f.requestPathScripts(t); got != 1 {
		t.Fatalf("request path ran %d metering scripts, want exactly 1 (%v)", got, f.store.scriptNames())
	}
	f.waitPostFlight(t)
	if got := f.store.meteringScripts(); got != 2 {
		t.Fatalf("reserve + commit = %d scripts, want 2 (%v)", got, f.store.scriptNames())
	}
}

// TestTokenRequestCostsOneScript is the same budget for a token: the
// revocation check rides the Reserve rather than adding a call of its own.
func TestTokenRequestCostsOneScript(t *testing.T) {
	f := newHotPathFixture(t, true)
	f.arm()

	w := f.do(t, f.mint(t, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if got := f.requestPathScripts(t); got != 1 {
		t.Fatalf("request path ran %d metering scripts, want exactly 1 (%v)", got, f.store.scriptNames())
	}
	f.waitPostFlight(t)
	if got := f.store.meteringScripts(); got != 2 {
		t.Fatalf("reserve + commit = %d scripts, want 2 (%v)", got, f.store.scriptNames())
	}
}

// TestTokenRequestWithNoRulesStillCostsOneScript is the zero-rule half of the
// invariant: nothing is metered, but the denylist check still has to run —
// and it must not turn into a commit for a reservation holding no state.
func TestTokenRequestWithNoRulesStillCostsOneScript(t *testing.T) {
	f := newHotPathFixture(t, false)
	f.arm()

	w := f.do(t, f.mint(t, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if got := f.requestPathScripts(t); got != 1 {
		t.Fatalf("request path ran %d metering scripts, want exactly 1 (%v)", got, f.store.scriptNames())
	}
	f.waitPostFlight(t)
	if got := f.store.meteringScripts(); got != 1 {
		t.Fatalf("commit added a script for a reservation with nothing to commit: %v", f.store.scriptNames())
	}
}

// TestKeyRequestWithNoRulesMakesNoMeteringCall is the other zero-rule case: a
// key carries no revocation check, so an unmetered policy reserves nothing.
func TestKeyRequestWithNoRulesMakesNoMeteringCall(t *testing.T) {
	f := newHotPathFixture(t, false)
	f.arm()

	w := f.do(t, "sk-wr-live")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	f.waitPostFlight(t)
	if got := f.store.meteringScripts(); got != 0 {
		t.Fatalf("unmetered key request ran %d metering scripts, want 0 (%v)", got, f.store.scriptNames())
	}
}

// TestRejectedCredentialsNeverTouchKV pins the second half of the rotation
// invariant: a credential that cannot be used is refused from the snapshot
// alone, so a flood of bad bearers never becomes load on the kv backend.
func TestRejectedCredentialsNeverTouchKV(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	disabled := false

	for _, tc := range []struct {
		name   string
		edit   func(*key.Key)
		bearer func(testing.TB, *hotPathFixture) string
		status int
	}{
		{
			name:   "unknown key",
			bearer: func(testing.TB, *hotPathFixture) string { return "sk-wr-nonesuch" },
			status: http.StatusUnauthorized,
		},
		{
			name:   "revoked key",
			edit:   func(k *key.Key) { k.Spec.RevokedAt = &past },
			bearer: func(testing.TB, *hotPathFixture) string { return "sk-wr-live" },
			status: http.StatusUnauthorized,
		},
		{
			name:   "expired key",
			edit:   func(k *key.Key) { k.Spec.ExpiresAt = &past },
			bearer: func(testing.TB, *hotPathFixture) string { return "sk-wr-live" },
			status: http.StatusUnauthorized,
		},
		{
			name:   "disabled key",
			edit:   func(k *key.Key) { k.Spec.Enabled = &disabled },
			bearer: func(testing.TB, *hotPathFixture) string { return "sk-wr-live" },
			status: http.StatusUnauthorized,
		},
		{
			name: "key whose rotation grace has closed",
			edit: func(k *key.Key) {
				closed := time.Now().Add(-time.Minute)
				k.Spec.PreviousKeyHash = sha("sk-wr-previous")
				k.Spec.GraceUntil = &closed
			},
			bearer: func(testing.TB, *hotPathFixture) string { return "sk-wr-previous" },
			status: http.StatusUnauthorized,
		},
		{
			name: "expired token",
			bearer: func(t testing.TB, f *hotPathFixture) string {
				return f.mint(t, func(c *crypto.TokenClaims) { c.Exp = c.Iat - 1 })
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "token from a bulk revocation",
			bearer: func(t testing.TB, f *hotPathFixture) string {
				return f.mint(t, func(c *crypto.TokenClaims) { c.Ver = 99 })
			},
			status: http.StatusUnauthorized,
		},
		{
			name: "token naming a project that is gone",
			bearer: func(t testing.TB, f *hotPathFixture) string {
				return f.mint(t, func(c *crypto.TokenClaims) { c.Prj = meta.NewID() })
			},
			status: http.StatusForbidden,
		},
		{
			name:   "unsigned bearer shaped like a token",
			bearer: func(testing.TB, *hotPathFixture) string { return "eyJhbGciOiJub25lIn0.eyJpc3MiOiJyZWxheSJ9.x" },
			status: http.StatusUnauthorized,
		},
		{
			name:   "no bearer at all",
			bearer: func(testing.TB, *hotPathFixture) string { return "" },
			status: http.StatusUnauthorized,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHotPathFixture(t, true)
			bearer := tc.bearer(t, f)
			if tc.edit != nil {
				f.rebuildKey(t, tc.edit)
			}
			f.arm()

			w := f.do(t, bearer)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			if got := f.store.totalOps(); got != 0 {
				t.Fatalf("rejection performed %d kv operations, want 0", got)
			}
			if f.upstreamCalls.Load() != 0 {
				t.Fatal("a rejected credential reached the upstream")
			}
		})
	}
}

// TestDepsInterfaceFieldsAreReviewed guards the structural half of "no
// Postgres on the request path". Scanning for pgx types would prove nothing:
// Deps.Pinger is an interface that internal/storage satisfies, so the data
// plane already holds a database handle — /healthz may call it, /v1/* may
// not. What can be enforced is that the set of interface-typed fields (the
// only shape such a handle can enter by) stays the reviewed one, so a new
// seam has to be argued against the hot-path rule rather than merged quietly.
func TestDepsInterfaceFieldsAreReviewed(t *testing.T) {
	want := map[string]bool{"Pinger": true}

	got := map[string]bool{}
	rt := reflect.TypeOf(Deps{})
	for i := 0; i < rt.NumField(); i++ {
		if f := rt.Field(i); f.Type.Kind() == reflect.Interface {
			got[f.Name] = true
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("Deps.%s is a new interface-typed field; it can carry a "+
				"database handle onto the request path — review it, then add it here", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("Deps.%s is gone; drop it from the reviewed set", name)
		}
	}
}
