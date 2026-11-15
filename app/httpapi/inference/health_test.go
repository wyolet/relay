package inference

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"

	appcatalog "github.com/wyolet/relay/app/catalog"
	"github.com/wyolet/relay/app/hostkey"
	"github.com/wyolet/relay/app/httpapi"
	"github.com/wyolet/relay/app/key"
	"github.com/wyolet/relay/pkg/catalogkind/binding"
	"github.com/wyolet/relay/pkg/catalogkind/host"
	"github.com/wyolet/relay/pkg/catalogkind/model"
	"github.com/wyolet/relay/pkg/catalogkind/policy"
	"github.com/wyolet/relay/pkg/catalogkind/pricing"
	"github.com/wyolet/relay/pkg/catalogkind/provider"
	"github.com/wyolet/relay/pkg/catalogkind/ratelimit"
)

type stubPinger struct{ err error }

func (p stubPinger) Ping(context.Context) error { return p.err }

// togglePinger fails with whatever error is stored, and answers while none is.
type togglePinger struct{ err atomic.Pointer[error] }

func (p *togglePinger) Ping(context.Context) error {
	if e := p.err.Load(); e != nil {
		return *e
	}
	return nil
}

type emptyRows[T any] struct{}

func (emptyRows[T]) List(context.Context) ([]*T, error) { return nil, nil }

func emptyCatalog() *appcatalog.Catalog {
	return appcatalog.New(emptyRows[provider.Provider]{}, emptyRows[host.Host]{}, emptyRows[policy.Policy]{},
		emptyRows[model.Model]{}, emptyRows[hostkey.HostKey]{}, emptyRows[ratelimit.RateLimit]{},
		emptyRows[key.Key]{}, emptyRows[pricing.Pricing]{}, emptyRows[binding.Binding]{})
}

// healthProbe is one GET /healthz: what changed before it, and the answer. A 503's body is the error envelope, checked on its own below.
type healthProbe struct {
	load   bool  // install the first snapshot before the probe
	pg     error // what Postgres answers to this probe
	status int
	body   string
}

// /healthz is readiness: 503 until the first snapshot is installed (an empty catalog still installs one) and while Postgres fails before the pod was first ready. After that a PG outage must not pull the pod from the Service, since the data plane serves from the snapshot, so pg is no longer pinged or reported. /livez answers ok throughout: a backend blip must never get a healthy pod killed.
func TestHealth(t *testing.T) {
	pgDown := errors.New("pg down")
	for _, tc := range []struct {
		name    string
		catalog bool // the deployment has a catalog
		pinger  bool
		probes  []healthProbe
	}{
		{name: "no backends", probes: []healthProbe{{status: 200, body: `{"status":"ok","backends":{}}`}}},
		{name: "postgres alone", pinger: true, probes: []healthProbe{
			{pg: pgDown, status: 503},
			{status: 200, body: `{"status":"ok","backends":{"pg":"ok"}}`},
			{pg: pgDown, status: 200, body: `{"status":"ok","backends":{}}`},
		}},
		{name: "catalog alone", catalog: true, probes: []healthProbe{
			{status: 503},
			{load: true, status: 200, body: `{"status":"ok","backends":{"catalog":"ok"}}`},
		}},
		{name: "catalog and postgres", catalog: true, pinger: true, probes: []healthProbe{
			{status: 503},
			{load: true, pg: pgDown, status: 503},
			{status: 200, body: `{"status":"ok","backends":{"catalog":"ok","pg":"ok"}}`},
			{pg: pgDown, status: 200, body: `{"status":"ok","backends":{"catalog":"ok"}}`},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d Deps
			cat, pg := emptyCatalog(), &togglePinger{}
			if tc.catalog {
				d.Catalog = cat
			}
			if tc.pinger {
				d.Pinger = pg
			}
			_, api := humatest.New(t)
			registerHealth(api, d)
			for i, probe := range tc.probes {
				if probe.load {
					if err := cat.Reload(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				pg.err.Store(nil)
				if probe.pg != nil {
					pg.err.Store(&probe.pg)
				}
				resp := api.Get("/healthz")
				if resp.Code != probe.status {
					t.Fatalf("probe %d: status %d, want %d", i, resp.Code, probe.status)
				}
				if probe.body != "" {
					wantJSON(t, "healthz", resp.Body.String(), probe.body)
				}
				live := api.Get("/livez")
				if live.Code != http.StatusOK {
					t.Fatalf("probe %d: /livez = %d, want 200", i, live.Code)
				}
				wantJSON(t, "livez", live.Body.String(), `{"status":"ok"}`)
			}
		})
	}
}

func TestHealthz_NotReadyIsAServerError(t *testing.T) {
	t.Skip(`bug: /healthz before the first snapshot → 503 {"error":{"type":"invalid_request_error","message":…}}, no code: relay's own unreadiness typed as the caller's fault`)
	httpapi.InstallErrorRewriter()
	_, api := humatest.New(t)
	registerHealth(api, Deps{Catalog: emptyCatalog()})
	resp := api.Get("/healthz")
	if env := decodeEnvelope(t, resp.Body.Bytes()); resp.Code != http.StatusServiceUnavailable || env.Type != "server_error" || env.Code == "" {
		t.Fatalf("status %d, envelope %+v, want a 503 server_error with a code", resp.Code, env)
	}
}
