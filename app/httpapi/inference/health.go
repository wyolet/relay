package inference

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

type healthzOutput struct {
	Body struct {
		Status   string            `json:"status" enum:"ok,degraded" doc:"Overall health verdict."`
		Backends map[string]string `json:"backends" doc:"Per-backend status; \"ok\" or an error string. catalog is \"loading\" until the first snapshot is installed; pg is reported only until the pod is first ready."`
	}
}

type livezOutput struct {
	Body struct {
		Status string `json:"status" enum:"ok" doc:"Process liveness."`
	}
}

func registerHealth(api huma.API, d Deps) {
	// /livez is process-only: it returns 200 as long as the HTTP server is
	// serving, with NO backend dependency. Liveness probes point here so a
	// transient PG/DNS blip never gets a healthy pod killed — that would
	// cascade restarts at the exact moment the backend is stressed. Backend
	// health and catalog loading belong on readiness/startup.
	huma.Register(api, huma.Operation{
		OperationID: "livez",
		Method:      "GET",
		Path:        "/livez",
		Summary:     "Process liveness (no backend dependency)",
		Tags:        []string{"system"},
	}, func(_ context.Context, _ *struct{}) (*livezOutput, error) {
		out := &livezOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	// Postgres gates readiness only until the pod is first ready. After that the data plane serves from the in-memory snapshot, and a shared-PG blip would otherwise pull every pod from the Service at once; PG health stays on the control port's /api/healthz.
	var wasReady atomic.Bool
	huma.Register(api, huma.Operation{
		OperationID: "healthz",
		Method:      "GET",
		Path:        "/healthz",
		Summary:     "Readiness: a loaded catalog, and Postgres until first ready",
		Tags:        []string{"system"},
	}, func(ctx context.Context, _ *struct{}) (*healthzOutput, error) {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		backends := map[string]string{}
		status := "ok"
		// Inference refuses every request until the first snapshot is installed, so the pod is not ready before then. An empty catalog still installs a snapshot and counts as ready.
		if d.Catalog != nil {
			if d.Catalog.IsReady() {
				backends["catalog"] = "ok"
			} else {
				backends["catalog"] = "loading"
				status = "degraded"
			}
		}
		if d.Pinger != nil && !wasReady.Load() {
			if err := d.Pinger.Ping(pingCtx); err != nil {
				backends["pg"] = "error: " + err.Error()
				status = "degraded"
			} else {
				backends["pg"] = "ok"
			}
		}

		if status == "ok" {
			wasReady.Store(true)
		}

		out := &healthzOutput{}
		out.Body.Status = status
		out.Body.Backends = backends
		if status != "ok" {
			return out, huma.Error503ServiceUnavailable("not ready: a backend is unhealthy or the catalog is still loading")
		}
		return out, nil
	})
}
