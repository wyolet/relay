package pipeline_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/adapters"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/meta"
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
