package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/httpapi"
)

type versionOutput struct {
	Body struct {
		Version string `json:"version" doc:"Relay build version."`
	}
}

func registerVersion(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "version",
		Method:      "GET",
		Path:        "/version",
		Summary:     "Relay build version",
		Tags:        []string{"system"},
	}, func(_ context.Context, _ *struct{}) (*versionOutput, error) {
		out := &versionOutput{}
		out.Body.Version = httpapi.Version
		return out, nil
	})
}

type masterKeyGenerateOutput struct {
	Body struct {
		Key string `json:"key" doc:"32 random bytes, base64-encoded. Set as RELAY_MASTER_KEY in the deployment env."`
	}
}

func registerMisc(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "master_key_generate",
		Method:      "POST",
		Path:        "/master-key/generate",
		Summary:     "Generate a new 32-byte master key for stored-mode HostKey encryption",
		Description: "Returns a candidate key — the server does NOT install it. " +
			"Operator copies the value into RELAY_MASTER_KEY and restarts the " +
			"deployment. Rotation requires re-encrypting all stored HostKey values.",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(_ context.Context, _ *struct{}) (*masterKeyGenerateOutput, error) {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, huma.Error500InternalServerError("rand: " + err.Error())
		}
		out := &masterKeyGenerateOutput{}
		out.Body.Key = base64.StdEncoding.EncodeToString(buf)
		return out, nil
	})

	type reloadOutput struct {
		Body struct {
			Status string `json:"status"`
		}
	}
	huma.Register(api, huma.Operation{
		OperationID: "reload",
		Method:      "POST",
		Path:        "/reload",
		Summary:     "Force a full catalog reload from Postgres",
		Description: "Rebuilds the in-memory snapshot from PG. Normally unnecessary " +
			"— NOTIFY/LISTEN propagates writes within ~1s. Use this if you suspect " +
			"the snapshot has drifted (e.g. after a manual DB edit).",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(ctx context.Context, _ *struct{}) (*reloadOutput, error) {
		if d.Catalog == nil {
			return nil, huma.Error500InternalServerError("catalog not wired")
		}
		if err := d.Catalog.Reload(ctx); err != nil {
			return nil, huma.Error500InternalServerError("reload: " + err.Error())
		}
		out := &reloadOutput{}
		out.Body.Status = "ok"
		return out, nil
	})
}
