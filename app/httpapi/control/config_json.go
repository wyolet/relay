// GET /config.json — the public, unauthenticated runtime config the embedded
// admin UI fetches once at boot, same-origin (it is served from the control
// listener, which is also where the SPA is served from). Modelled on the OIDC
// `/.well-known/openid-configuration` discovery pattern.
//
// Why runtime, not build-time: relay-ui is built once into a generic tarball
// and go:embed'd into the binary, so VITE_* build-time env can't encode a
// specific deployment's URLs/flags. The UI learns them here at boot instead.
//
// WORLD-READABLE — public values only. Never add secrets (keys, DSNs to private
// services, internal hostnames). The body is rendered once at registration from
// static process env, so it is effectively a constant for the process lifetime.
package control

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/httpapi"
)

// RuntimeConfig is the JSON shape the UI reads. Every field is optional and
// omitempty: an empty controlApiUrl tells the UI to use its own origin; an
// empty inferenceApiUrl has no safe origin default (UI/data-plane may differ),
// so the UI prompts rather than guessing. The object is additive — the UI
// reads keys it knows and ignores the rest.
type RuntimeConfig struct {
	ControlAPIURL   string          `json:"controlApiUrl,omitempty"`
	InferenceAPIURL string          `json:"inferenceApiUrl,omitempty"`
	Mode            string          `json:"mode,omitempty"` // "oss" | "cloud"
	Version         string          `json:"version,omitempty"`
	Features        map[string]bool `json:"features,omitempty"`
	Telemetry       *Telemetry      `json:"telemetry,omitempty"`
	DocsURL         string          `json:"docsUrl,omitempty"`
	SupportURL      string          `json:"supportUrl,omitempty"`
}

// Telemetry carries PUBLIC client-side telemetry config only.
type Telemetry struct {
	SentryDSN   string `json:"sentryDsn,omitempty"`
	Environment string `json:"environment,omitempty"`
}

// Typed Body (not raw bytes) so the response schema lands in OpenAPI as
// RuntimeConfig — the UI's generated client picks up a typed shape on regen.
// huma adds a "$schema" link key to the body like every other control
// endpoint; the UI ignores keys it doesn't know, so the agreed shape holds.
type configJSONOutput struct {
	CacheControl string        `header:"Cache-Control"`
	Body         RuntimeConfig `contentType:"application/json"`
}

// registerConfigJSON installs the public GET /config.json. No protect
// middleware: the UI fetches it before it can authenticate. The body is
// fixed from static process env — a constant for the process lifetime.
func registerConfigJSON(api huma.API, d Deps) {
	body := d.RuntimeConfig
	if body.Version == "" {
		body.Version = httpapi.Version
	}
	resp := &configJSONOutput{
		// Cacheable but short — a deploy's new values land within a minute,
		// and refreshes within a session serve from cache. Never no-store.
		CacheControl: "public, max-age=60, stale-while-revalidate=600",
		Body:         body,
	}
	huma.Register(api, huma.Operation{
		OperationID: "runtime_config",
		Method:      "GET",
		Path:        "/config.json",
		Summary:     "Public runtime config for the embedded admin UI",
		Tags:        []string{"system"},
	}, func(_ context.Context, _ *struct{}) (*configJSONOutput, error) {
		return resp, nil
	})
}
