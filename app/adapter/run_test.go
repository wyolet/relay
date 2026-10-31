package adapter

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/keypool"
	"github.com/wyolet/relay/app/limits"
	"github.com/wyolet/relay/app/limits/lua"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/pipeline"
	"github.com/wyolet/relay/app/plan"
	"github.com/wyolet/relay/pkg/kv"
	"github.com/wyolet/relay/pkg/lifecycle"
	pkgratelimit "github.com/wyolet/relay/pkg/ratelimit"
)

// postFlightDone signals the end of post-flight: Reachable is its last step.
type postFlightDone chan struct{}

func (d postFlightDone) Reachable(context.Context, string)           { d <- struct{}{} }
func (d postFlightDone) Unreachable(context.Context, string, string) {}

// runFixture is a pipeline over one key whose post-flight end is observable.
type runFixture struct {
	p    *pipeline.Pipeline
	done postFlightDone
	req  func(a pipeline.Adapter, baseURL string, body []byte) *pipeline.Request
}

func newRunFixture(tb testing.TB) *runFixture {
	tb.Helper()
	mem := kv.NewMem()
	tb.Cleanup(func() { _ = mem.Close() })
	pkgratelimit.RegisterScripts(mem)
	keypool.RegisterScripts(mem)
	done := make(postFlightDone, 1)
	cred := &hostkey.HostKey{Meta: meta.Metadata{ID: "key-1", Name: "key-1"}, Resolved: "sk-test", KeyHash: "hash-1"}
	mdl := &model.Model{Meta: meta.Metadata{ID: "model-1", Name: "m1"}}
	return &runFixture{
		p: &pipeline.Pipeline{
			Limiter:    lua.New(pkgratelimit.New(mem, slog.Default(), nil)),
			Selector:   keypool.New(mem, slog.Default(), nil, nil),
			Lifecycle:  lifecycle.New(),
			HostHealth: done,
		},
		done: done,
		req: func(a pipeline.Adapter, baseURL string, body []byte) *pipeline.Request {
			hst := &host.Host{Meta: meta.Metadata{ID: "host-1", Name: "upstream"}, Spec: host.Spec{BaseURL: baseURL}}
			slot := &plan.KeySlot{Credential: cred, Host: hst}
			pp := &plan.PoolPlan{Host: hst, Slots: []*plan.KeySlot{slot}, Selection: keypool.KeySelectionPrioritized}
			return &pipeline.Request{
				Body: body, Adapter: a, Model: mdl,
				Hops:      []plan.RouteHop{{Pool: pp, Host: hst, Slots: pp.Slots, Counters: [][]limits.Counter{nil}}},
				Lifecycle: lifecycle.NewContext("req", "pipeline", time.Now()),
			}
		},
	}
}

func newRunSpec(sync time.Duration) *Spec {
	s := (&Spec{Name: adapters.OpenAI, DefaultPath: "/v1/chat/completions", Auth: AuthStrategy{Header: "Authorization", Scheme: "Bearer"}}).Build()
	if sync > 0 {
		s.syncTimeout = sync
	}
	return s
}

// The sync cap still bounds a body the upstream stalls on after its headers, now that it rides on the pipeline's call context; post-flight still ends.
func TestRunSyncTimeoutCoversBodyRead(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
	}))
	defer up.Close()
	defer close(release)

	f := newRunFixture(t)
	res, err := f.p.Run(context.Background(), f.req(newRunSpec(150*time.Millisecond).PipelineAdapter(), up.URL, []byte(`{}`)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	start := time.Now()
	if _, err := io.ReadAll(res.Body); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled body read err = %v, want the deadline", err)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("stalled body read took %v, want about the 150ms cap", waited)
	}
	_ = res.Body.Close()
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
		t.Fatal("post-flight never finished")
	}
}

// An upstream that never answers is cut at the sync cap with the deadline error.
func TestRunSyncTimeoutBeforeHeaders(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer up.Close()
	defer close(release)

	f := newRunFixture(t)
	_, err := f.p.Run(context.Background(), f.req(newRunSpec(150*time.Millisecond).PipelineAdapter(), up.URL, []byte(`{}`)))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
}

// A client that disconnects mid-call aborts the upstream request at once, not at the sync cap.
func TestRunClientDisconnectAbortsUpstream(t *testing.T) {
	gone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The server notices a closed connection only once the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		close(gone)
	}))
	defer up.Close()

	f := newRunFixture(t)
	ctx, leave := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, leave)
	_, err := f.p.Run(ctx, f.req(newRunSpec(time.Minute).PipelineAdapter(), up.URL, []byte(`{}`)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request still open after the client left")
	}
}

// BenchmarkRun is one buffered request through the pipeline over the spec adapter: the upstream call, the response read and close, and post-flight to its end. A 1.5 KiB answer arrives in one read; a 24 KiB one in several.
func BenchmarkRun(b *testing.B) {
	for _, size := range []int{1500, 24 << 10} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			answer := append(append([]byte(`{"id":"x","text":"`), bytes.Repeat([]byte("y"), size)...), `"}`...)
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Length", strconv.Itoa(len(answer)))
				_, _ = w.Write(answer)
			}))
			b.Cleanup(up.Close)

			f := newRunFixture(b)
			a := newRunSpec(0).PipelineAdapter()
			body := bytes.Repeat([]byte("x"), 3<<10)
			ctx := context.Background()

			b.ReportAllocs()
			for b.Loop() {
				res, err := f.p.Run(ctx, f.req(a, up.URL, body))
				if err != nil {
					b.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
				<-f.done
			}
		})
	}
}
