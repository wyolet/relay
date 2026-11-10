package control

import (
	"context"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	appcatalog "github.com/wyolet/relay/app/catalog"
)

type catalogSyncOutput struct {
	Body struct {
		Previous string `json:"previous" doc:"Catalog release the database was seeded from before the check."`
		Current  string `json:"current" doc:"Catalog release the database is seeded from after it."`
		Changed  bool   `json:"changed" doc:"Whether the check seeded a newer release."`
	}
}

func registerCatalogSync(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "catalog_sync",
		Method:      "POST",
		Path:        "/catalog/sync",
		Summary:     "Check for a newer catalog release now",
		Description: "Reads the catalog index and, when it names a release newer than the seeded one, " +
			"fetches, verifies and seeds it before answering, as the periodic check does. " +
			"Only a relay with RELAY_CATALOG_VERSION=latest checks; any other answers 409.",
		Tags:        []string{"system"},
		Middlewares: protect,
		Errors:      []int{401, 403, 409, 502, 503},
	}, func(ctx context.Context, _ *struct{}) (*catalogSyncOutput, error) {
		return syncCatalog(ctx, d)
	})
}

func syncCatalog(ctx context.Context, d Deps) (*catalogSyncOutput, error) {
	if err := d.Authz.Authorize(ctx, "system.catalog-sync", authz.Resource{Kind: "system"}); err != nil {
		return nil, mapAuthzErr(err)
	}
	if d.CatalogSync == nil {
		return nil, huma.Error409Conflict("this relay does not follow the catalog: it checks for releases only with RELAY_CATALOG_VERSION=latest")
	}
	res, err := d.CatalogSync(ctx)
	if errors.Is(err, appcatalog.ErrSeedBusy) {
		return nil, huma.Error503ServiceUnavailable("another relay is seeding the catalog now; the catalog-source settings section shows its result")
	}
	if err != nil {
		return nil, huma.Error502BadGateway("catalog check failed; the seeded catalog is unchanged: " + err.Error())
	}
	out := &catalogSyncOutput{}
	out.Body.Previous = res.Previous
	out.Body.Current = res.Current
	out.Body.Changed = res.Changed
	return out, nil
}
