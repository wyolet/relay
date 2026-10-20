package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/adapters"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/usagelog"
	"github.com/wyolet/relay/pkg/clientprofile"
	"github.com/wyolet/relay/pkg/httpheader"
	"github.com/wyolet/relay/pkg/lifecycle"
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
func gatedRouter(adm *httpapi.Admission) (chi.Router, *bool) {
	r := chi.NewRouter()
	api := humachi.New(r, huma.DefaultConfig("test", "1"))
	reached := new(bool)
	huma.Register(api, huma.Operation{
		OperationID: "gated",
		Method:      http.MethodPost,
		Path:        "/gated",
		Middlewares: huma.Middlewares{shedWhenFull(adm)},
	}, func(context.Context, *struct{}) (*struct{}, error) {
		*reached = true
		return nil, nil
	})
	return r, reached
}

func TestShedWhenFull_PassesThroughBelowCap(t *testing.T) {
	r, reached := gatedRouter(admissionAt(t, false))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/gated", strings.NewReader(`{}`)))
	if !*reached {
		t.Fatalf("handler not reached below the cap (status %d)", w.Code)
	}
	if w.Code == http.StatusTooManyRequests {
		t.Fatal("shed below the cap")
	}
}

func TestShedWhenFull_RefusesAtCapWithoutReadingBody(t *testing.T) {
	r, reached := gatedRouter(admissionAt(t, true))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/gated", unreadBody{t}))

	if *reached {
		t.Fatal("handler reached at the cap")
	}
	assertShed429(t, w)
	if got := w.Header().Get("Connection"); got != "close" {
		t.Errorf("Connection = %q, want close", got)
	}
}

// assertShed429 checks the refusal both shed stages write.
func assertShed429(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", w.Code, w.Body)
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
	if env.Err.Type != "rate_limit_error" || env.Err.Code != "overloaded" || env.Err.Message == "" {
		t.Errorf("envelope = %+v, want rate_limit_error/overloaded with a message", env.Err)
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
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("POST %s at the cap: status %d, want 429", path, w.Code)
		}
	}
	for _, path := range []string{"/healthz", "/v1/models"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code == http.StatusTooManyRequests {
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

	assertShed429(t, w)
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
	mw := shedWhenFull(httpapi.NewAdmission(1))
	hctx := humachi.NewContext(&huma.Operation{}, httptest.NewRequest(http.MethodPost, "/", nil), httptest.NewRecorder())
	next := func(huma.Context) {}
	b.ReportAllocs()
	for b.Loop() {
		mw(hctx, next)
	}
}
