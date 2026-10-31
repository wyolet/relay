package inference

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/app/ratelimit"
	"github.com/wyolet/relay/app/rolebinding"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/kv/kvtest"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// proxyStreamDeps wires proxy mode over plans with "test-model" on host "anthropic" at upstreamURL, a user policy with a generous request limit, and a passthrough key of that user. Scripts run on a recording kv.Mem.
func proxyStreamDeps(tb testing.TB, upstreamURL string) (Deps, *Principal, *kvtest.Recording) {
	tb.Helper()
	return proxyStreamDepsLimited(tb, upstreamURL, 1<<40)
}

// proxyStreamDepsLimited is proxyStreamDeps with the policy allowing perHour requests; system rate limits join the catalog.
func proxyStreamDepsLimited(tb testing.TB, upstreamURL string, perHour int64, system ...*ratelimit.RateLimit) (Deps, *Principal, *kvtest.Recording) {
	tb.Helper()
	var w plantest.World
	prov := w.Provider("anthropic")
	h := w.Host("anthropic", upstreamURL)
	m := w.Model(prov, "test-model")
	w.Bind(m, h, adapters.Anthropic)
	w.Credential(h, nil, plantest.System, "sk-dummy")
	rl := w.RateLimit(ratelimit.Rule{Meter: ratelimit.MeterRequests, Amount: perHour, Window: ratelimit.Window(time.Hour), Strategy: ratelimit.StrategyFixedWindow})
	u := w.User()
	k := w.UserKey(u, "stream-secret")
	k.Meta.Name = "rk"
	k.Spec.PassthroughAllowed = true
	pol := w.Policy("p", policy.Spec{RateLimits: []policy.RateLimitLink{{RateLimitID: rl.Meta.ID}}})
	w.BindPolicy(pol, plantest.Subject(rolebinding.SubjectUser, u.ID))
	plans := w.Load(tb)
	cat := catalogtest.Catalog{Providers: w.Providers, Hosts: w.Hosts, Models: w.Models, Bindings: w.Bindings, HostKeys: w.Credentials, Keys: w.Keys, RateLimits: system}.Load(tb)

	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	tb.Cleanup(func() { _ = mem.Close() })
	store := kvtest.NewRecording(mem)
	reg := buildTestRegistry()
	d := Deps{Catalog: cat, Plans: plans.Holder, Proxy: proxy.New(lua.New(pkgratelimit.New(store, slog.New(slog.DiscardHandler), nil)), nil, nil), Adapters: reg.AdapterMap(), Specs: reg}
	kp := plans.Root().Keys.Lookup(k.Spec.KeyHash, time.Now())
	if kp == nil {
		tb.Fatal("fixture key has no plan")
	}
	return d, principalOf(kp, k.Spec.KeyHash), store
}

// withPayloadLogging is pr under a copy of its innermost caller policy that captures payloads.
func withPayloadLogging(pr *Principal) *Principal {
	caller := *pr.Caller
	for i := len(caller.Rungs) - 1; i >= 0; i-- {
		if ep := caller.Rungs[i].Policy; ep != nil {
			pol := *ep.Policy
			pol.Spec.PayloadLoggingEnabled = true
			epc := *ep
			epc.Policy = &pol
			caller.Rungs[i].Policy = &epc
			break
		}
	}
	cp := *pr
	cp.Caller = &caller
	return &cp
}

// proxyShapeCall is one proxy-mode request through the shape route. A nil principal is anonymous; length is the declared Content-Length, -1 for unknown.
type proxyShapeCall struct {
	principal *Principal
	pin       string
	body      io.Reader
	length    int64
}

func serveProxyShape(d Deps, req proxyShapeCall) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", req.body)
	r.ContentLength = req.length
	mode := ModeProxyAnonymous
	if req.principal != nil {
		mode = ModeProxyAuthed
	}
	ctx := WithClassification(r.Context(), Classification{Mode: mode, Key: "relay-key", UpstreamAuth: "Bearer upstream-key", UpstreamHost: req.pin, ClientIP: "203.0.113.9"})
	if req.principal != nil {
		ctx = withPrincipal(ctx, d.Catalog.Current(), req.principal, req.principal.Key)
	}
	w := httptest.NewRecorder()
	handleShape(d.Specs.Spec(adapters.Anthropic), d, w, r.WithContext(ctx))
	return w
}

// postFlightSeen is what a post-flight hook read off a request's lifecycle context.
type postFlightSeen struct {
	requestBody    []byte
	truncated      bool
	requestBytes   int
	requestedModel string
	errorKind      string
}

// watchPostFlight registers a hook on d's proxy runner that reports each request's post-flight view.
func watchPostFlight(d *Deps) <-chan postFlightSeen {
	seen := make(chan postFlightSeen, 8)
	reg := lifecycle.New()
	reg.RegisterHook(lifecycle.HookFunc{HookName: "seen", Fn: func(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
		seen <- postFlightSeen{lc.RequestBody, lc.RequestBodyTruncated, lc.RequestBytes, lc.RequestedModel, ev.ErrorKind}
		return nil, nil
	}})
	d.Proxy.Lifecycle = reg
	return seen
}

func awaitPostFlight(tb testing.TB, seen <-chan postFlightSeen) postFlightSeen {
	tb.Helper()
	select {
	case s := <-seen:
		return s
	case <-time.After(5 * time.Second):
		tb.Fatal("post-flight never ran")
		return postFlightSeen{}
	}
}

// upstreamGot is what the test upstream received.
type upstreamGot struct {
	body          []byte
	readErr       error
	contentLength int64
}

// recordingUpstream answers every request with a small message after reading the whole body, reporting what it received.
func recordingUpstream(tb testing.TB) (*httptest.Server, <-chan upstreamGot) {
	tb.Helper()
	got := make(chan upstreamGot, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		got <- upstreamGot{body: b, readErr: err, contentLength: r.ContentLength}
		_, _ = w.Write([]byte(`{"id":"m","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	tb.Cleanup(srv.Close)
	return srv, got
}

// claudeCodeSizedBody is a coding-agent-like chat body of about size bytes: this package's Go sources as tool results between short turns, with the model first or after the messages.
func claudeCodeSizedBody(tb testing.TB, size int, modelLast bool) []byte {
	tb.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		tb.Fatalf("no source files: %v", err)
	}
	var msgs []any
	for i, n := 0, 0; n < size; i++ {
		src, err := os.ReadFile(files[i%len(files)])
		if err != nil {
			tb.Fatal(err)
		}
		msgs = append(msgs,
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": fmt.Sprintf("t%d", i), "name": "Read", "input": map[string]any{"file_path": files[i%len(files)]}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("t%d", i), "content": string(src)}}},
		)
		n += len(src)
	}
	rest, err := json.Marshal(map[string]any{"max_tokens": 4096, "system": "You are a coding agent.", "messages": msgs})
	if err != nil {
		tb.Fatal(err)
	}
	if modelLast {
		return append(rest[:len(rest)-1], `,"model":"test-model"}`...)
	}
	return append([]byte(`{"model":"test-model",`), rest[1:]...)
}
