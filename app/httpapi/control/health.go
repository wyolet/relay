package control

import (
	"context"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
)

// ControlHealth is the /healthz body, the same on 200 and 503.
type ControlHealth struct {
	Status              string            `json:"status" enum:"ok,degraded" doc:"Overall health verdict."`
	Backends            map[string]string `json:"backends" doc:"Status per backend, keyed pg (the database) and redis (sessions and rate limits, absent when state is in memory). Each value is \"ok\" or \"error\"."`
	MasterKeyConfigured *bool             `json:"master_key_configured,omitempty" doc:"Whether a master key is set, so stored host keys can be encrypted. Present for authenticated callers only."`
}

type healthzOutput struct {
	Status int
	Body   ControlHealth
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

	healthSchema := api.OpenAPI().Components.Schemas.Schema(reflect.TypeOf(ControlHealth{}), true, "")
	huma.Register(api, huma.Operation{
		OperationID: "control_healthz",
		Method:      "GET",
		Path:        "/healthz",
		Summary:     "Readiness: process + backend health",
		Tags:        []string{"system"},
		Responses: map[string]*huma.Response{
			"503": {
				Description: "A backend is unreachable",
				Content:     map[string]*huma.MediaType{"application/json": {Schema: healthSchema}},
			},
		},
	}, func(ctx context.Context, _ *struct{}) (*healthzOutput, error) {
		out := &healthzOutput{Status: http.StatusOK}
		out.Body = pingBackends(ctx, d.HealthChecks)
		// The endpoint is public; how secrets are held is for signed-in callers only, as with /version's license block.
		if actor.From(ctx).IsAuthenticated() {
			out.Body.MasterKeyConfigured = &d.MasterKeyConfigured
		}
		if out.Body.Status != "ok" {
			out.Status = http.StatusServiceUnavailable
		}
		return out, nil
	})
}

// pingBackends pings every backend concurrently so one hung backend can't use up another's timeout.
func pingBackends(ctx context.Context, checks map[string]Pinger) ControlHealth {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	h := ControlHealth{Status: "ok", Backends: make(map[string]string, len(checks))}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, p := range checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := p.Ping(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// The endpoint is public, so the cause goes to the log rather than the body.
				slog.Error("control healthz: backend ping failed", "backend", name, "err", err)
				h.Backends[name] = "error"
				h.Status = "degraded"
				return
			}
			h.Backends[name] = "ok"
		}()
	}
	wg.Wait()
	return h
}
