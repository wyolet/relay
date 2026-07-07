package inference

import (
	"context"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

type healthzOutput struct {
	Body struct {
		Status   string            `json:"status" enum:"ok,degraded" doc:"Overall health verdict."`
		Backends map[string]string `json:"backends" doc:"Per-backend status; \"ok\" or an error string."`
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
	// transient PG/DNS blip (which degrades /healthz to 503) never gets a
	// healthy pod killed — that would cascade restarts at the exact moment
	// the backend is stressed. Backend health belongs on readiness/startup.
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

	huma.Register(api, huma.Operation{
		OperationID: "healthz",
		Method:      "GET",
		Path:        "/healthz",
		Summary:     "Readiness: process + backend health",
		Tags:        []string{"system"},
	}, func(ctx context.Context, _ *struct{}) (*healthzOutput, error) {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		backends := map[string]string{}
		status := "ok"
		if d.Pinger != nil {
			if err := d.Pinger.Ping(pingCtx); err != nil {
				backends["pg"] = "error: " + err.Error()
				status = "degraded"
			} else {
				backends["pg"] = "ok"
			}
		}

		out := &healthzOutput{}
		out.Body.Status = status
		out.Body.Backends = backends
		if status != "ok" {
			return out, huma.Error503ServiceUnavailable("backend(s) unhealthy")
		}
		return out, nil
	})
}
