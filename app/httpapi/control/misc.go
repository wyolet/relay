package control

import (
	"context"

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
