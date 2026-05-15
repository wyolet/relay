package control

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/settings"
)

// registerSettings wires the /settings endpoints. Each known section
// gets its own typed GET/PUT pair so the generated OpenAPI carries a
// concrete schema (no opaque JSON). The list endpoint returns every
// section as a raw JSON message — enumerating all typed variants in a
// single response shape would explode the spec.
//
// Section discovery: GET /settings/sections returns the metadata
// catalog (name + description + schema $ref). The section names are
// also exposed as a string-enum schema (settings.SectionName) so any
// client can typecheck inputs against the closed set.
func registerSettings(api huma.API, d Deps, protect huma.Middlewares) {
	registerSettingsSection[settings.ProxyMode](api, d, protect, settings.Section{
		Name:        settings.SectionProxyMode,
		Description: "Proxy-mode flow gate. Controls whether the relay accepts inference requests where the caller supplies their own upstream provider key.",
	})
	registerSettingsSection[settings.Inference](api, d, protect, settings.Section{
		Name:        settings.SectionInference,
		Description: "Authenticated /v1/* behavior. AllowMissingPolicy lets RelayKeys with no Spec.PolicyID reach any host the relay has hostkeys for, bypassing the per-policy authorization gate. Default off.",
	})
	registerSettingsList(api, d, protect)
	registerSettingsSectionsCatalog(api, d, protect)
}

type settingsListItem struct {
	Section settings.SectionName `json:"section"`
	Value   json.RawMessage      `json:"value"`
}

// settingsCatalogItem describes one registered section. SchemaRef points
// into #/components/schemas/ so clients can resolve the typed shape.
type settingsCatalogItem struct {
	Name        settings.SectionName `json:"name"`
	Description string               `json:"description,omitempty"`
	SchemaRef   string               `json:"schemaRef,omitempty" doc:"OpenAPI component name for this section's typed value."`
}

type settingsCatalogOutput struct {
	Body struct {
		Items []settingsCatalogItem `json:"items"`
	}
}

func registerSettingsSectionsCatalog(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "list_settings_sections",
		Method:      "GET",
		Path:        "/settings/sections",
		Summary:     "List registered settings sections and their schemas",
		Description: "Enumerates every settings section known to this relay " +
			"build, with the OpenAPI component name of its typed value. Use " +
			"alongside the SectionName enum to discover what /settings/{section} " +
			"endpoints exist.",
		Tags:        []string{"settings"},
		Middlewares: protect,
		Errors:      []int{401},
	}, func(ctx context.Context, _ *struct{}) (*settingsCatalogOutput, error) {
		out := &settingsCatalogOutput{}
		names := settings.Names()
		out.Body.Items = make([]settingsCatalogItem, 0, len(names))
		for _, n := range names {
			sec, _ := settings.Lookup(n)
			out.Body.Items = append(out.Body.Items, settingsCatalogItem{
				Name:        settings.SectionName(n),
				Description: sec.Description,
				SchemaRef:   sec.SchemaRef,
			})
		}
		return out, nil
	})
}

type settingsListOutput struct {
	Body struct {
		Items []settingsListItem `json:"items"`
	}
}

func registerSettingsList(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "list_settings",
		Method:      "GET",
		Path:        "/settings",
		Summary:     "List all settings sections",
		Description: "Returns every registered settings section with its " +
			"current value as raw JSON. Use the per-section GET to retrieve " +
			"a typed shape.",
		Tags:        []string{"settings"},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(ctx context.Context, _ *struct{}) (*settingsListOutput, error) {
		if err := d.Authz.Authorize(ctx, "settings.list", authz.Resource{Kind: "settings"}); err != nil {
			return nil, mapAuthzErr(err)
		}
		rows, err := d.Stores.Settings.List(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &settingsListOutput{}
		out.Body.Items = make([]settingsListItem, 0, len(rows))
		for _, r := range rows {
			raw, err := json.Marshal(r.Value)
			if err != nil {
				return nil, huma.Error500InternalServerError(
					fmt.Sprintf("marshal %s: %s", r.Section, err))
			}
			out.Body.Items = append(out.Body.Items, settingsListItem{
				Section: settings.SectionName(r.Section),
				Value:   raw,
			})
		}
		return out, nil
	})
}

// sectionEnvelope is the named (non-anonymous) Body type for a typed
// section response. Lifting it out of sectionResponse keeps the OpenAPI
// schema id stable across generic instantiations — anonymous Body
// structs produce hint-based names that don't match the emitted $ref.
type sectionEnvelope[T any] struct {
	Section settings.SectionName `json:"section"`
	Value   T                    `json:"value"`
}

// sectionResponse is the typed wrapper returned by GET/PUT for one
// section. Declared at package level so each generic instantiation has
// a stable type name (same reason as the CRUD wrappers in crud.go).
type sectionResponse[T any] struct {
	Body sectionEnvelope[T]
}

type sectionUpdateRequest[T any] struct {
	Body T `json:"body"`
}

// registerSettingsSection wires GET and PUT for one typed section. T is
// the section's Go struct; sec carries the key, description, and is
// updated in place with the resolved OpenAPI schema id so the sections
// catalog endpoint can surface it to clients.
func registerSettingsSection[T any](api huma.API, d Deps, protect huma.Middlewares, sec settings.Section) {
	section := sec.Name
	path := "/settings/" + section

	huma.Register(api, huma.Operation{
		OperationID: "get_settings_" + section,
		Method:      "GET",
		Path:        path,
		Summary:     "Get settings section: " + section,
		Description: sec.Description,
		Tags:        []string{"settings"},
		Middlewares: protect,
		Errors:      []int{401, 500},
	}, func(ctx context.Context, _ *struct{}) (*sectionResponse[T], error) {
		if err := d.Authz.Authorize(ctx, "settings.read", authz.Resource{Kind: "settings", Name: section}); err != nil {
			return nil, mapAuthzErr(err)
		}
		row, err := d.Stores.Settings.Get(ctx, section)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &sectionResponse[T]{}
		out.Body.Section = settings.SectionName(section)
		if v, ok := row.Value.(*T); ok {
			out.Body.Value = *v
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update_settings_" + section,
		Method:      "PUT",
		Path:        path,
		Summary:     "Update settings section: " + section,
		Description: sec.Description,
		Tags:        []string{"settings"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(ctx context.Context, in *sectionUpdateRequest[T]) (*sectionResponse[T], error) {
		if err := d.Authz.Authorize(ctx, "settings.update", authz.Resource{Kind: "settings", Name: section}); err != nil {
			return nil, mapAuthzErr(err)
		}
		raw, err := json.Marshal(in.Body)
		if err != nil {
			return nil, huma.Error400BadRequest("encode: " + err.Error())
		}
		row, err := d.Stores.Settings.Upsert(ctx, section, raw)
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		out := &sectionResponse[T]{}
		out.Body.Section = settings.SectionName(section)
		if v, ok := row.Value.(*T); ok {
			out.Body.Value = *v
		}
		return out, nil
	})

	// Record the OpenAPI component name of T against this section so
	// /settings/sections can surface it. The same naming rule the global
	// schemaNamer uses applies here — defer to the registry by inspecting
	// the registered schema name for *T.
	settings.SetSchemaRef(section, schemaRefFor[T](api))
}

// schemaRefFor returns the OpenAPI component name huma assigned to T,
// e.g. "ProxyMode". The registry's Schema(t, true, hint) returns a
// *Schema whose Ref is "#/components/schemas/<name>" for any non-
// primitive that got registered as a component.
func schemaRefFor[T any](api huma.API) string {
	reg := api.OpenAPI().Components.Schemas
	if reg == nil {
		return ""
	}
	s := reg.Schema(reflect.TypeOf(*new(T)), true, "")
	if s == nil || s.Ref == "" {
		return ""
	}
	const prefix = "#/components/schemas/"
	if strings.HasPrefix(s.Ref, prefix) {
		return s.Ref[len(prefix):]
	}
	return s.Ref
}
