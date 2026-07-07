package inference

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
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
