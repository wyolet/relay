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

func registerHealth(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "healthz",
		Method:      "GET",
		Path:        "/healthz",
		Summary:     "Liveness + backend readiness",
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
