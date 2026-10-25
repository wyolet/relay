package inference

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	"github.com/wyolet/relay/app/binding"
	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/host"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/app/model"
	"github.com/wyolet/relay/app/policy"
	"github.com/wyolet/relay/app/pricing"
	"github.com/wyolet/relay/app/provider"
	"github.com/wyolet/relay/app/ratelimit"
)

type stubPinger struct{ err error }

func (p stubPinger) Ping(context.Context) error { return p.err }

// /livez is process-only: it must return 200 regardless of backend health, so a
// PG/DNS blip never trips the liveness probe and gets a healthy pod killed.
func TestLivez_ProcessOnly_IgnoresBackend(t *testing.T) {
	_, api := humatest.New(t)
	registerHealth(api, Deps{Pinger: stubPinger{err: errors.New("pg down")}})

	resp := api.Get("/livez")
	if resp.Code != http.StatusOK {
		t.Fatalf("/livez with a failing backend: got %d, want 200", resp.Code)
	}
}

// /healthz is readiness: a failing backend degrades it to 503 so the pod is
// pulled from the Service endpoints (but NOT killed).
func TestHealthz_BackendFailure_Is503(t *testing.T) {
	_, api := humatest.New(t)
	registerHealth(api, Deps{Pinger: stubPinger{err: errors.New("pg down")}})

	resp := api.Get("/healthz")
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz with a failing backend: got %d, want 503", resp.Code)
	}
}

func TestHealthz_BackendOK_Is200(t *testing.T) {
	_, api := humatest.New(t)
	registerHealth(api, Deps{Pinger: stubPinger{}})

	resp := api.Get("/healthz")
	if resp.Code != http.StatusOK {
		t.Fatalf("/healthz with a healthy backend: got %d, want 200", resp.Code)
	}
}

type emptyRows[T any] struct{}

func (emptyRows[T]) List(context.Context) ([]*T, error) { return nil, nil }

func emptyCatalog() *appcatalog.Catalog {
	return appcatalog.New(emptyRows[provider.Provider]{}, emptyRows[host.Host]{}, emptyRows[policy.Policy]{},
		emptyRows[model.Model]{}, emptyRows[hostkey.HostKey]{}, emptyRows[ratelimit.RateLimit]{},
		emptyRows[key.Key]{}, emptyRows[pricing.Pricing]{}, emptyRows[binding.Binding]{})
}

// Inference answers 503 catalog_unavailable until the first snapshot is installed, so readiness
// must fail until then; liveness must not, or a slow catalog load gets the pod killed.
func TestHealthz_NotReadyUntilCatalogLoads(t *testing.T) {
	cat := emptyCatalog()
	_, api := humatest.New(t)
	registerHealth(api, Deps{Pinger: stubPinger{}, Catalog: cat})

	if resp := api.Get("/healthz"); resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz before the first snapshot: got %d, want 503", resp.Code)
	}
	if resp := api.Get("/livez"); resp.Code != http.StatusOK {
		t.Fatalf("/livez before the first snapshot: got %d, want 200", resp.Code)
	}

	// An empty catalog (seeding off, no rows) still installs a snapshot and is ready.
	if err := cat.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resp := api.Get("/healthz"); resp.Code != http.StatusOK {
		t.Fatalf("/healthz after the first snapshot: got %d, want 200 (body %s)", resp.Code, resp.Body.String())
	}
}

type togglePinger struct{ err atomic.Pointer[error] }

func (p *togglePinger) Ping(context.Context) error {
	if e := p.err.Load(); e != nil {
		return *e
	}
	return nil
}

func (p *togglePinger) fail(err error) { p.err.Store(&err) }

// Postgres gates readiness during startup only: once the pod has been ready, a PG
// outage must not pull it from the Service, since the data plane serves from the snapshot.
func TestHealthz_PGIgnoredAfterFirstReady(t *testing.T) {
	cat := emptyCatalog()
	pg := &togglePinger{}
	pg.fail(errors.New("pg down"))
	_, api := humatest.New(t)
	registerHealth(api, Deps{Pinger: pg, Catalog: cat})

	if err := cat.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if resp := api.Get("/healthz"); resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("/healthz with PG down before first ready: got %d, want 503", resp.Code)
	}

	pg.err.Store(nil)
	if resp := api.Get("/healthz"); resp.Code != http.StatusOK {
		t.Fatalf("/healthz once PG answers: got %d, want 200", resp.Code)
	}

	pg.fail(errors.New("pg down"))
	resp := api.Get("/healthz")
	if resp.Code != http.StatusOK {
		t.Fatalf("/healthz with PG down after first ready: got %d, want 200 (body %s)", resp.Code, resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), `"pg"`) {
		t.Fatalf("/healthz after first ready still reports pg: %s", resp.Body.String())
	}
}
