package pipeline_test

import (
	"bytes"
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/meta"
	pkgusage "github.com/wyolet/relay/sdk/usage"
	v1 "github.com/wyolet/relay/sdk/v1"
)

// Each hop is sent HopBody's form of the base body for that hop, asked once per hop however many of its credentials are tried, and a failover to a hop that rewrites nothing sends the base untouched.
func TestHopBody_PerHop(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	got := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path] = append(got[r.URL.Path], string(b))
		mu.Unlock()
		if r.URL.Path == "/a/v1/generate" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	adp := (&adapter.Spec{
		Name:        "test",
		DefaultPath: "/v1/generate",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
	}).Build().PipelineAdapter()
	hostAt := func(name string) *host.Host {
		return &host.Host{Meta: meta.Metadata{ID: name, Name: name}, Spec: host.Spec{BaseURL: srv.URL + "/" + name}}
	}

	calls := map[string]int{}
	const base = `{"max_tokens":5}`
	body := []byte(base)
	req := &pipeline.Request{
		Body:      body,
		Adapter:   adp,
		BodyShape: "test",
		HopBody: func(hop *plan.RouteHop, shape adapters.Name, b []byte) ([]byte, []string) {
			calls[hop.Host.Meta.Name]++
			if hop.Host.Meta.Name != "a" || shape != "test" {
				return b, nil
			}
			return []byte(`{"max_completion_tokens":5}`), []string{"max_tokens"}
		},
		Hops: []plan.RouteHop{
			hopAt(hostAt("a"), []*hostkey.HostKey{makeKey("a1", "sk-a1"), makeKey("a2", "sk-a2")}),
			hopAt(hostAt("b"), []*hostkey.HostKey{makeKey("b1", "sk-b1")}),
		},
	}
	res, err := newPipeline().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drainResult(t, res)

	mu.Lock()
	defer mu.Unlock()
	want := map[string][]string{
		"/a/v1/generate": {`{"max_completion_tokens":5}`, `{"max_completion_tokens":5}`},
		"/b/v1/generate": {base},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("bodies per host (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]int{"a": 1, "b": 1}, calls); diff != "" {
		t.Errorf("HopBody calls per hop (-want +got):\n%s", diff)
	}
	if res.BodyDropped != nil {
		t.Errorf("BodyDropped = %v, want none: the serving hop rewrote nothing", res.BodyDropped)
	}
	if string(body) != base {
		t.Errorf("base body modified to %s", body)
	}
}

// usageTranslator reads every response as one reporting input tokens.
type usageTranslator struct {
	v1.Translator
	input int64
}

func (u usageTranslator) ParseResponse([]byte) (*v1.Response, error) {
	return &v1.Response{Usage: pkgusage.Tokens{"input": u.input}}, nil
}

// With HopAdapters set each hop is called with its own binding's adapter, path and auth, its body built and its host path override picked for its own shape, and post-flight reads the response with the adapter that served it.
func TestHopAdapters_PerHop(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	type call struct{ Path, Auth, Key string }
	var got []call
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, call{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Key")})
		mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/a/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	specs := adapter.NewRegistry(
		(&adapter.Spec{Name: "shape-a", DefaultPath: "/v1/a", Auth: adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"}, Translator: usageTranslator{input: 1}}).Build(),
		(&adapter.Spec{Name: "shape-b", Family: "shape-a", DefaultPath: "/v1/b", Auth: adapter.AuthStrategy{Header: "X-Key"}, Translator: usageTranslator{input: 2}}).Build(),
	)
	hopVia := func(name string, shape adapters.Name, spec host.Spec) plan.RouteHop {
		spec.BaseURL = srv.URL + "/" + name
		return hopBound(name, shape, spec)
	}
	shapes := map[string]adapters.Name{}
	reg, events := usageRegistry()
	p := newPipeline()
	p.Lifecycle = reg
	res, err := p.Run(context.Background(), &pipeline.Request{
		Body:        []byte(`{}`),
		Adapter:     specs.PipelineAdapter("shape-a"),
		HopAdapters: specs,
		BodyShape:   "shape-a",
		HopBody: func(hop *plan.RouteHop, shape adapters.Name, b []byte) ([]byte, []string) {
			shapes[hop.Host.Meta.Name] = shape
			return b, nil
		},
		Hops: []plan.RouteHop{
			hopVia("a", "shape-a", host.Spec{Paths: map[adapters.Name]string{"shape-b": "/custom/b"}}),
			hopVia("b", "shape-b", host.Spec{Paths: map[adapters.Name]string{"shape-b": "/custom/b"}}),
		},
		Lifecycle: lifecycle.NewContext("req", "pipeline", time.Now()),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drainResult(t, res)

	mu.Lock()
	defer mu.Unlock()
	want := []call{{"/a/v1/a", "Bearer sk-a", ""}, {"/b/custom/b", "", "sk-b"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("upstream calls (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(map[string]adapters.Name{"a": "shape-a", "b": "shape-b"}, shapes); diff != "" {
		t.Errorf("HopBody shape per hop (-want +got):\n%s", diff)
	}
	if ev := events.next(t); !maps.Equal(ev.Tokens, pkgusage.Tokens{"input": 2}) {
		t.Errorf("usage tokens = %v, want the serving adapter's reading {input: 2}", ev.Tokens)
	}
}

// hopBound is one hop at a host named name holding the credential "sk-"+name, its binding in shape.
func hopBound(name string, shape adapters.Name, spec host.Spec) plan.RouteHop {
	hop := hopAt(&host.Host{Meta: meta.Metadata{ID: name, Name: name}, Spec: spec}, []*hostkey.HostKey{makeKey(name, "sk-"+name)})
	hop.Binding = &binding.Binding{Spec: binding.Spec{Adapter: shape}}
	return hop
}

// adapterMap is a pipeline.Request.HopAdapters over fixed adapters.
type adapterMap map[adapters.Name]pipeline.Adapter

func (m adapterMap) PipelineAdapter(name adapters.Name) pipeline.Adapter { return m[name] }

// timedOrgAdapter caps its calls at an hour and reads the organization "org-b" off every response.
type timedOrgAdapter struct{ *fakeAdapter }

func (timedOrgAdapter) CallTimeout(bool) time.Duration { return time.Hour }
func (timedOrgAdapter) OrgID(http.Header) string       { return "org-b" }

type orgsSeen struct{ seen []string }

func (o *orgsSeen) ObserveOrg(accountID, credentialID, orgID string) {
	o.seen = append(o.seen, accountID+"/"+orgID)
}

// With HopAdapters set the hop's own adapter, not Request.Adapter, classifies its failure, caps its call and reads its organization.
func TestHopAdapters_ClassifyTimeoutOrgPerHop(t *testing.T) {
	t.Parallel()

	unused := &fakeAdapter{callFn: func(context.Context, string, string, []byte, http.Header) (*http.Response, error) {
		t.Error("Request.Adapter was called")
		return errResp(http.StatusInternalServerError), nil
	}}
	failing := &fakeAdapter{
		callFn: func(context.Context, string, string, []byte, http.Header) (*http.Response, error) {
			return errResp(http.StatusInternalServerError), nil
		},
		retryFn: func(*http.Response) (bool, keypool.FailureKind, time.Duration) {
			return true, keypool.FailureServerError, 0
		},
	}
	var capped bool
	serving := timedOrgAdapter{&fakeAdapter{callFn: func(ctx context.Context, _, _ string, _ []byte, _ http.Header) (*http.Response, error) {
		dl, ok := ctx.Deadline()
		capped = ok && time.Until(dl) > 30*time.Minute
		return okResp("ok"), nil
	}}}
	orgs := &orgsSeen{}
	p := newPipeline()
	p.Orgs = orgs

	res, err := p.Run(context.Background(), &pipeline.Request{
		Body:        []byte(`{}`),
		Adapter:     unused,
		HopAdapters: adapterMap{"shape-a": failing, "shape-b": serving},
		Hops:        []plan.RouteHop{hopBound("a", "shape-a", host.Spec{}), withAccount(hopBound("b", "shape-b", host.Spec{}), "acct-b")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drainResult(t, res)

	if res.Status != http.StatusOK || res.KeyHash != "b" {
		t.Errorf("served %d by %q, want 200 by b: the first hop's 500 must be classified retryable by its own adapter", res.Status, res.KeyHash)
	}
	if !capped {
		t.Error("the serving call carried no hour-long deadline: its timeout came from another adapter")
	}
	if diff := cmp.Diff([]string{"acct-b/org-b"}, orgs.seen); diff != "" {
		t.Errorf("observed orgs (-want +got):\n%s", diff)
	}
}

// streamTranslator reads a canonical stream as is; a blind one reads nothing from any stream.
type streamTranslator struct {
	v1.IdentityTranslator
	blind bool
}

func (s streamTranslator) NewToCanonicalStream() func([]byte) ([]byte, error) {
	if s.blind {
		return func([]byte) ([]byte, error) { return nil, nil }
	}
	return nil
}

// After a failover between hops whose shapes read their streams differently, the stream is summarized with the serving hop's translator, not the one the request started with.
func TestHopAdapters_StreamSummarizedByServingHop(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/a/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: generation.completed\ndata: {\"usage\":{\"input\":5}}\n\n")
	}))
	defer srv.Close()

	blind := streamTranslator{blind: true}
	specs := adapter.NewRegistry(
		(&adapter.Spec{Name: "shape-a", DefaultPath: "/v1/a", Translator: blind}).Build(),
		(&adapter.Spec{Name: "shape-b", DefaultPath: "/v1/b", Translator: streamTranslator{}}).Build(),
	)
	reg, events := usageRegistry()
	p := newPipeline()
	p.Lifecycle = reg
	lc := lifecycle.NewContext("req", "pipeline", time.Now())
	lc.Translator = blind
	res, err := p.Run(context.Background(), &pipeline.Request{
		Body:        []byte(`{}`),
		Adapter:     specs.PipelineAdapter("shape-a"),
		HopAdapters: specs,
		Stream:      true,
		Hops:        []plan.RouteHop{hopBound("a", "shape-a", host.Spec{BaseURL: srv.URL + "/a"}), hopBound("b", "shape-b", host.Spec{BaseURL: srv.URL + "/b"})},
		Lifecycle:   lc,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drainResult(t, res)

	if ev := events.next(t); !maps.Equal(ev.Tokens, pkgusage.Tokens{"input": 5}) {
		t.Errorf("usage tokens = %v, want {input: 5} as the serving hop's translator reads the stream", ev.Tokens)
	}
}

// A retried attempt must still send the whole body: releasing it is only allowed once a response is accepted.
func TestRetry_SecondAttemptSendsFullBody(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	got := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.Header.Get("Authorization")] = b
		mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer sk-1" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	adp := (&adapter.Spec{
		Name:        "test",
		DefaultPath: "/v1/generate",
		Auth:        adapter.AuthStrategy{Header: "Authorization", Scheme: "Bearer"},
	}).Build().PipelineAdapter()

	body := bytes.Repeat([]byte("x"), 256<<10)
	req := &pipeline.Request{
		Body:    body,
		Adapter: adp,
		Hops:    []plan.RouteHop{hopAt(&host.Host{Meta: meta.Metadata{ID: "h", Name: "h"}, Spec: host.Spec{BaseURL: srv.URL}}, []*hostkey.HostKey{makeKey("h1", "sk-1"), makeKey("h2", "sk-2")})},
	}
	res, err := newPipeline().Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	drainResult(t, res)

	if res.KeyHash != "h2" {
		t.Fatalf("KeyHash = %q, want h2 (failover)", res.KeyHash)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, auth := range []string{"Bearer sk-1", "Bearer sk-2"} {
		if !bytes.Equal(got[auth], body) {
			t.Fatalf("%s: upstream got %d bytes, want %d", auth, len(got[auth]), len(body))
		}
	}
	if req.Body != nil {
		t.Fatal("pipeline still references the request body after the response was accepted")
	}
}
