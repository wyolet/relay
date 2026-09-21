package control

import (
	"context"
	"encoding/json"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/license"
	"github.com/wyolet/relay/app/settings"
)

type licenseOutput struct {
	Body license.Info
}

type licenseInput struct {
	Body struct {
		Value string `json:"value" doc:"The signed license file, verbatim. Empty clears the stored license (the deployment falls back to the environment, else community)."`
	}
}

// licenseInfo is the live license summary, or the community zero value when
// no license service is wired.
func licenseInfo(d Deps) license.Info {
	if d.License == nil {
		return license.Info{}
	}
	return d.License.Info()
}

func registerLicense(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "get_license",
		Method:      "GET",
		Path:        "/license",
		Summary:     "Read the live license",
		Description: "Summarizes the license this process verified at boot. " +
			"An unlicensed deployment returns licensed=false; every gated " +
			"feature reports license_required.",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{401, 403},
	}, func(ctx context.Context, _ *struct{}) (*licenseOutput, error) {
		if err := d.Authz.Authorize(ctx, "license.read", authz.Resource{Kind: "license"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		return &licenseOutput{Body: licenseInfo(d)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "put_license",
		Method:      "PUT",
		Path:        "/license",
		Summary:     "Install a license without a redeploy",
		Description: "Verifies the value offline and, on success, stores it in " +
			"the `license` settings section and makes it live. A value that " +
			"does not verify is rejected and the previous license stays in " +
			"place. RELAY_LICENSE_FILE / RELAY_LICENSE still win when set.",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 500},
	}, func(ctx context.Context, in *licenseInput) (*licenseOutput, error) {
		if err := d.Authz.Authorize(ctx, "license.update", authz.Resource{Kind: "license"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		if d.License == nil {
			return nil, huma.Error500InternalServerError("license service not wired")
		}
		info, err := d.License.Set(in.Body.Value)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if d.Stores == nil || d.Stores.Settings == nil {
			return nil, huma.Error500InternalServerError("settings store not wired")
		}
		raw, err := json.Marshal(settings.License{Value: in.Body.Value})
		if err != nil {
			return nil, huma.Error500InternalServerError("marshal: " + err.Error())
		}
		audit.Changed(ctx, []string{"license.value"})
		if _, err := d.Stores.Settings.Upsert(ctx, settings.SectionLicense, raw); err != nil {
			return nil, huma.Error500InternalServerError("settings: " + err.Error())
		}
		return &licenseOutput{Body: info}, nil
	})
}
