package control

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

type healthzOutput struct {
	Status int
	Body   struct {
		Status   string            `json:"status" enum:"ok,degraded" doc:"Overall health verdict."`
		Backends map[string]string `json:"backends" doc:"Per-backend status: \"ok\" or \"error\"."`
	}
}

type livezOutput struct {
	Body struct {
		Status string `json:"status" enum:"ok" doc:"Process liveness."`
	}
}

func registerHealth(api huma.API, d Deps) {
	// Process-only, no backend dependency: a transient PG blip must not trip a liveness probe.
	huma.Register(api, huma.Operation{
		OperationID: "control_livez",
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
		OperationID: "control_healthz",
		Method:      "GET",
		Path:        "/healthz",
		Summary:     "Readiness: process + backend health",
		Tags:        []string{"system"},
	}, func(ctx context.Context, _ *struct{}) (*healthzOutput, error) {
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		out := &healthzOutput{Status: http.StatusOK}
		out.Body.Status = "ok"
		out.Body.Backends = map[string]string{}
		if d.Pinger != nil {
			// The endpoint is public, so the cause goes to the log rather than the body.
			if err := d.Pinger.Ping(pingCtx); err != nil {
				slog.Error("control healthz: backend ping failed", "backend", "pg", "err", err)
				out.Body.Backends["pg"] = "error"
				out.Body.Status = "degraded"
				out.Status = http.StatusServiceUnavailable
			} else {
				out.Body.Backends["pg"] = "ok"
			}
		}
		return out, nil
	})
}
