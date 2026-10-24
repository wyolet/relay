package inference

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/wyolet/relay/app/adapter"
	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
	"github.com/wyolet/relay/pkg/metrics"
)

// admissionAt returns a one-slot Admission, with its slot held when full.
func admissionAt(t testing.TB, full bool) *httpapi.Admission {
	t.Helper()
	adm := httpapi.NewAdmission(1)
	if full {
		lc := lifecycle.NewContext("holder", "pipeline", time.Now())
		if err := adm.PreFlight(context.Background(), lc, &lifecycle.PreFlightEvent{}); err != nil {
			t.Fatalf("hold the slot: %v", err)
		}
	}
	return adm
}

// unreadBody fails the test when anything reads the request body.
type unreadBody struct{ t *testing.T }

func (b unreadBody) Read([]byte) (int, error) {
	b.t.Error("request body was read")
	return 0, io.EOF
}

// gatedRouter mounts one POST /gated operation behind shedWhenFull and reports whether its handler ran.
func gatedRouter(adm *httpapi.Admission, kvPoolBusy func() bool) (chi.Router, *bool) {
	r := chi.NewRouter()
	api := humachi.New(r, huma.DefaultConfig("test", "1"))
	reached := new(bool)
	huma.Register(api, huma.Operation{
		OperationID: "gated",
		Method:      http.MethodPost,
		Path:        "/gated",
		Middlewares: huma.Middlewares{shedWhenFull(adm, kvPoolBusy, &relayOverloaded)},
	}, func(context.Context, *struct{}) (*struct{}, error) {
		*reached = true
		return nil, nil
	})
	return r, reached
}

func TestShedWhenFull_PassesThroughBelowCap(t *testing.T) {
	r, reached := gatedRouter(admissionAt(t, false), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/gated", strings.NewReader(`{}`)))
	if !*reached {
		t.Fatalf("handler not reached below the cap (status %d)", w.Code)
	}
	if w.Code == http.StatusServiceUnavailable {
		t.Fatal("shed below the cap")
	}
}

func TestShedWhenFull_RefusesAtCapWithoutReadingBody(t *testing.T) {
	r, reached := gatedRouter(admissionAt(t, true), nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/gated", unreadBody{t}))

	if *reached {
		t.Fatal("handler reached at the cap")
	}
	assertOverloaded(t, w)
	if got := w.Header().Get("Connection"); got != "" {
		t.Errorf("Connection = %q, want none: a refusal must not force a reconnect", got)
	}
}

// scrapedValue reads one series from the /metrics exposition; 0 when it has no sample yet.
func scrapedValue(t *testing.T, series string) float64 {
	t.Helper()
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for line := range strings.Lines(w.Body.String()) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatalf("parse %s: %v", line, err)
			}
			return f
		}
	}
	return 0
}

// While the kv pool's wait queue is full the admission reserve would be refused unsent, so the route refuses first, before the body read, counted as a kv_pool shed: no reserve was attempted.
func TestShedWhenFull_RefusesWhileKVPoolBusyWithoutReadingBody(t *testing.T) {
	const shedSeries = `relay_requests_shed_total{stage="kv_pool"}`
	reserveShed := metrics.KVErrors.WithLabelValues("limit_reserve", metrics.KVShed)
	before, reserveBefore := scrapedValue(t, shedSeries), testutil.ToFloat64(reserveShed)
	busy := true
	r, reached := gatedRouter(admissionAt(t, false), func() bool { return busy })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/gated", unreadBody{t}))
	if *reached {
		t.Fatal("handler reached while the kv pool was busy")
	}
	assertOverloaded(t, w)
	if got := scrapedValue(t, shedSeries) - before; got != 1 {
		t.Fatalf("kv_pool shed counter moved by %v, want 1", got)
	}
	if got := testutil.ToFloat64(reserveShed) - reserveBefore; got != 0 {
		t.Fatalf("limit_reserve shed counter moved by %v, want 0: no reserve was sent", got)
	}

	busy = false
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/gated", strings.NewReader(`{}`)))
	if !*reached {
		t.Fatalf("handler not reached once the pool freed (status %d)", w.Code)
	}
}

// A refusal at the cap leaves a keep-alive connection usable: the unread body is drained by the server, and the next request on the same connection is served once a slot frees.
func TestShedWhenFull_KeepsTheConnection(t *testing.T) {
	adm := httpapi.NewAdmission(1)
	holder := lifecycle.NewContext("holder", "pipeline", time.Now())
	if err := adm.PreFlight(context.Background(), holder, &lifecycle.PreFlightEvent{}); err != nil {
		t.Fatalf("hold the slot: %v", err)
	}
	r, reached := gatedRouter(adm, nil)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	br := bufio.NewReader(conn)
	post := func() *http.Response {
		t.Helper()
		body := `{"pad":"` + strings.Repeat("a", 16<<10) + `"}`
		if _, err := fmt.Fprintf(conn, "POST /gated HTTP/1.1\r\nHost: relay\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
			t.Fatalf("write request: %v", err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("read response: %v", err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res
	}

	if res := post(); res.StatusCode != http.StatusServiceUnavailable || res.Close {
		t.Fatalf("at the cap: status %d close %v, want 503 on an open connection", res.StatusCode, res.Close)
	}
	adm.Collect(holder)
	if res := post(); res.StatusCode >= 300 || !*reached {
		t.Fatalf("next request on the same connection: status %d, handler reached %v", res.StatusCode, *reached)
	}
}

// assertOverloaded checks relay's own overload refusal, as both shed stages and an unreachable state store write it for a shape that declares none.
func assertOverloaded(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", w.Code, w.Body)
	}
	for header, want := range map[string]string{
		"Retry-After":                httpapi.RetryAfterShed,
		HeaderOrigin:                 "relay",
		"Content-Type":               "application/json",
		httpheader.HeaderShouldRetry: "true",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	var env httpapi.APIError
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the relay error envelope: %v (%s)", err, w.Body)
	}
	if env.Err.Type != "server_error" || env.Err.Code != "overloaded" || env.Err.Message == "" {
		t.Errorf("envelope = %+v, want server_error/overloaded with a message", env.Err)
	}
}

// Mount puts the capacity check on the generation routes, the profile-prefixed ones included, and nowhere else.
func TestMount_ShedsOnlyGenerationRoutes(t *testing.T) {
	cat, _ := buildDispatchCatalog(t, "anthropic", adapters.Anthropic)
	d := buildDeps(t, cat)
	d.Pinger = stubPinger{}
	profiles := clientprofile.New()
	if err := profiles.Register(stubProfile{}); err != nil {
		t.Fatalf("register profile: %v", err)
	}
	d.Profiles = profiles
	d.RouteMounters = []RouteMounter{MountRegistry(buildPathRegistry())}
	d.Admission = admissionAt(t, true)

	r := chi.NewRouter()
	Mount(r, d)

	for _, path := range []string{"/anthropic/v1/messages", "/stub/v1/messages"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, unreadBody{t}))
		assertSpecOverloaded(t, w)
	}
	for _, path := range []string{"/healthz", "/v1/models"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code == testOverloaded.Status {
			t.Errorf("GET %s shed at the cap", path)
		}
	}
}

// A request that passed the route check but lost the last slot to a concurrent one is refused by pre-flight: no usage event, no per-request log line, and the slots stay as they were.
func TestDispatch_LateShedSkipsUsageAndKeepsSlots(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	adm := httpapi.NewAdmission(1)
	holder := lifecycle.NewContext("holder", "pipeline", time.Now())
	if err := adm.PreFlight(context.Background(), holder, &lifecycle.PreFlightEvent{}); err != nil {
		t.Fatalf("hold the slot: %v", err)
	}

	events := make(usageCapture, 1)
	reg := lifecycle.New()
	reg.RegisterPreFlight(adm.PreFlight)
	reg.RegisterHook(usagelog.NewUsageHook(nil, ""))
	reg.RegisterCollector(adm)
	reg.RegisterCollector(events)

	body := []byte(`{"model":"m"}`)
	w := httptest.NewRecorder()
	Dispatch(Deps{Lifecycle: reg}, w, httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewReader(body)),
		DispatchInput{Inbound: adapters.OpenAI, Body: body, ModelName: "m"})

	assertOverloaded(t, w)
	if strings.Contains(logs.String(), "inference: error response") {
		t.Errorf("late shed logged a per-request line:\n%s", logs.String())
	}
	select {
	case ev := <-events:
		t.Fatalf("late shed emitted a usage event: %+v", ev)
	case <-time.After(100 * time.Millisecond):
	}

	if !adm.Full() {
		t.Fatal("the shed released the holder's slot")
	}
	adm.Collect(holder)
	if adm.Full() {
		t.Fatal("slot not returned by its holder")
	}
}

func BenchmarkShedWhenFull_PassThrough(b *testing.B) {
	mw := shedWhenFull(httpapi.NewAdmission(1), func() bool { return false }, &relayOverloaded)
	hctx := humachi.NewContext(&huma.Operation{}, httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
	next := func(huma.Context) {}
	b.ReportAllocs()
	for b.Loop() {
		mw(hctx, next)
	}
}

// testOverloaded stands in for a shape's own overload answer.
var testOverloaded = &adapter.ErrorResponse{Status: 529, Body: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)}

// assertSpecOverloaded checks a refusal answered with the shape's declared overload response and relay's retry headers.
func assertSpecOverloaded(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != testOverloaded.Status || w.Body.String() != string(testOverloaded.Body) {
		t.Fatalf("got %d %s, want the shape's %d %s", w.Code, w.Body, testOverloaded.Status, testOverloaded.Body)
	}
	for header, want := range map[string]string{
		"Retry-After":                httpapi.RetryAfterShed,
		HeaderOrigin:                 "relay",
		httpheader.HeaderShouldRetry: "true",
	} {
		if got := w.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

// A late shed answers in the inbound shape's own overload form when the shape declares one.
func TestDispatch_LateShedAnswersInTheInboundShape(t *testing.T) {
	adm := admissionAt(t, true)
	reg := lifecycle.New()
	reg.RegisterPreFlight(adm.PreFlight)
	reg.RegisterCollector(adm)

	body := []byte(`{"model":"m"}`)
	w := httptest.NewRecorder()
	Dispatch(Deps{Lifecycle: reg, Specs: buildPathRegistry()}, w, httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", bytes.NewReader(body)),
		DispatchInput{Inbound: adapters.Anthropic, Body: body, ModelName: "m"})
	assertSpecOverloaded(t, w)
}
