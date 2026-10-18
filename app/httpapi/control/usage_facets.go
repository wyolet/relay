package control

import (
	"context"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/pkg/usage"
)

type usageFacetsInput struct {
	UsageFilterInput
	Dims  []string `query:"dims" doc:"Required. Comma-separated dimensions to list values for, at most 16, no duplicates: any group_by value except \"none\" (e.g. \"model_id,host_id,tags.env\")."`
	Limit int      `query:"limit" doc:"Values returned per dimension. Default 50, max 200."`
}

type usageFacetsOutput struct {
	Body usage.FacetsResult
}

// validateFacetDims rejects an empty, oversized, duplicated or unknown dimension list.
func validateFacetDims(dims []string) error {
	if len(dims) == 0 {
		return huma.Error400BadRequest("`dims` is required (e.g. \"model_id,host_id\")")
	}
	if len(dims) > usage.MaxFacetDims {
		return huma.Error400BadRequest("`dims` lists more than " + strconv.Itoa(usage.MaxFacetDims) + " dimensions")
	}
	seen := make(map[string]bool, len(dims))
	for _, dim := range dims {
		if !usage.IsValidFacetDim(dim) {
			return huma.Error400BadRequest("invalid dim: " + dim)
		}
		if seen[dim] {
			return huma.Error400BadRequest("duplicate dim: " + dim)
		}
		seen[dim] = true
	}
	return nil
}

func registerUsageFacets(api huma.API, d Deps, protect huma.Middlewares) {
	if d.UsageReader == nil {
		return
	}
	huma.Register(api, huma.Operation{
		OperationID: "usage_facets",
		Method:      http.MethodGet,
		Path:        "/usage/facets",
		Summary:     "Top values per dimension for filter dropdowns",
		Description: "For each dimension in `dims`, returns its most frequent values over the filtered " +
			"stream with request counts, ordered by requests descending then value. Each dimension " +
			"ignores its own filter and keeps every other one, so model_id values honour host_id= but not model_id=. " +
			"Rows are counted like /usage/summary: requests rejected before reaching an upstream are excluded. " +
			"An id value carries a `label`: its latest non-empty slug in the window, else its current catalog name. " +
			"`from`/`to` echo the requested window.",
		Tags:        []string{"usage"},
		Middlewares: protect,
		Errors:      []int{400, 401, 500},
	}, func(ctx context.Context, in *usageFacetsInput) (*usageFacetsOutput, error) {
		base, err := in.toEventQuery()
		if err != nil {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if err := validateFacetDims(in.Dims); err != nil {
			return nil, err
		}
		if in.Limit < 0 || in.Limit > usage.MaxFacetLimit {
			return nil, huma.Error400BadRequest("`limit` must not be negative or above " + strconv.Itoa(usage.MaxFacetLimit))
		}
		sc, err := scopeOf(ctx, d.Authz, d.Catalog, "usage")
		if err != nil {
			return nil, err
		}
		from, to := requestedWindow(base)
		out := &usageFacetsOutput{Body: usage.FacetsResult{From: from, To: to}}
		if !scopeEventQuery(&base, sc) {
			out.Body.Facets = make(map[string][]usage.FacetValue, len(in.Dims))
			for _, dim := range in.Dims {
				out.Body.Facets[dim] = []usage.FacetValue{}
			}
			return out, nil
		}
		facets, err := usage.Facets(ctx, d.UsageReader, base, in.Dims, in.Limit)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		fillFacetLabels(d.Catalog, facets)
		out.Body.Facets = facets
		return out, nil
	})
}
