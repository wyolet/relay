package inference

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/catalog/catalogtest"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/app/plan/plantest"
	"github.com/wyolet/relay/app/proxy"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// anonymousHostDeps wires proxy mode with "test-model" on host "anthropic" at upstreamURL, its spec then changed by edits.
func anonymousHostDeps(tb testing.TB, upstreamURL string, edits ...func(*host.Spec)) Deps {
	tb.Helper()
	var w plantest.World
	prov := w.Provider("anthropic")
	h := w.Host("anthropic", upstreamURL)
	for _, edit := range edits {
		edit(&h.Spec)
	}
	w.Bind(w.Model(prov, "test-model"), h, adapters.Anthropic)
	plans := w.Load(tb)
	cat := catalogtest.Catalog{Providers: w.Providers, Hosts: w.Hosts, Models: w.Models, Bindings: w.Bindings}.Load(tb)
	mem := kv.NewMem()
	pkgratelimit.RegisterScripts(mem)
	tb.Cleanup(func() { _ = mem.Close() })
	reg := buildTestRegistry()
	return Deps{Catalog: cat, Plans: plans.Holder, Proxy: proxy.New(lua.New(pkgratelimit.New(mem, slog.New(slog.DiscardHandler), nil)), nil, nil), Adapters: reg.AdapterMap(), Specs: reg}
}

// Anonymous proxy traffic names no caller and a host is no policy subject, so a pinned request goes through whatever its model, known to the catalog or not.
func TestProxyShape_AnonymousPinGoesThrough(t *testing.T) {
	for _, model := range []string{"test-model", "unknown-model"} {
		t.Run(model, func(t *testing.T) {
			up, got := recordingUpstream(t)
			d := anonymousHostDeps(t, up.URL)
			body := proxyTestBody(model, 16, false)
			w := serveProxyShape(d, proxyShapeCall{pin: "anthropic", body: bytes.NewReader(body), length: int64(len(body))})
			if w.Code != http.StatusOK || len(got) != 1 {
				t.Fatalf("status %d, upstream reached %d times: %s", w.Code, len(got), w.Body)
			}
			if g := <-got; !bytes.Equal(g.body, body) || g.readErr != nil {
				t.Fatalf("upstream got %q (%v), want the body as sent", g.body, g.readErr)
			}
		})
	}
}

// proxyFailure is what a proxy request's failure record carries.
type proxyFailure struct {
	Kind, Refusal, Message, Host string
	Status                       int
}

// Proxy mode fills a host path's placeholders from the host's backend, and refuses before sending anything when one is unfilled, recording the failure as the pipeline does.
func TestProxyShape_HostPathPlaceholders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		backend  map[string]string
		status   int
		code     string
		wantURIs []string
		failure  *proxyFailure
	}{
		{"filled", map[string]string{"project": "p 1"}, http.StatusOK, "", []string{"/v1/projects/p%201/messages"}, nil},
		{"unfilled", map[string]string{"location": "global"}, http.StatusBadGateway, plan.CodeHostMisconfigured, nil, &proxyFailure{
			Kind: "host_misconfigured", Refusal: "host_not_reachable", Host: "anthropic", Status: 502,
			Message: `upstream host "anthropic" is misconfigured: its backend sets no value for the path placeholder {project}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var uris []string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				uris = append(uris, r.RequestURI)
				mu.Unlock()
				_, _ = w.Write([]byte(`{"id":"m","type":"message","usage":{"input_tokens":1,"output_tokens":1}}`))
			}))
			t.Cleanup(up.Close)
			d := anonymousHostDeps(t, up.URL, func(s *host.Spec) {
				path := "/v1/projects/{project}/messages"
				s.Path = &path
				s.Backend = tc.backend
			})
			failures := make(chan proxyFailure, 4)
			d.Lifecycle = lifecycle.New()
			d.Lifecycle.RegisterHook(lifecycle.HookFunc{HookName: "failure", Fn: func(lc *lifecycle.Context, ev *lifecycle.PostFlightEvent) (any, error) {
				failures <- proxyFailure{ev.ErrorKind, ev.Refusal, ev.ErrorMessage, lc.HostName, ev.Status}
				return nil, nil
			}})
			body := proxyTestBody("test-model", 16, false)
			r := proxyShapeCall{pin: "anthropic", body: bytes.NewReader(body), length: int64(len(body))}.request(d)
			r = r.WithContext(lifecycle.ContextWith(r.Context(), lifecycle.NewContext("req", "proxy", time.Now())))
			w := httptest.NewRecorder()
			handleShape(d.Specs.Spec(adapters.Anthropic), d, w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body)
			}
			if tc.failure != nil {
				if got := errCode(t, w.Body.Bytes()); got != tc.code {
					t.Errorf("code %q, want %q", got, tc.code)
				}
				select {
				case got := <-failures:
					if got != *tc.failure {
						t.Errorf("failure record = %+v, want %+v", got, *tc.failure)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("no failure record")
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if diff := cmp.Diff(tc.wantURIs, uris); diff != "" {
				t.Errorf("upstream requests (-want +got):\n%s", diff)
			}
		})
	}
}
