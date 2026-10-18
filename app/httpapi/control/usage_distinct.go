package control

import (
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/pkg/usage"
)

// checkDistinct is a 400 for a distinct dimension outside usage.ValidDistinct, raised before the query reaches a store.
func checkDistinct(dim string) error {
	if dim == "" {
		return nil
	}
	if _, ok := usage.DistinctColumn(dim); !ok {
		return huma.Error400BadRequest("invalid distinct: " + dim + " (want one of " + strings.Join(usage.ValidDistinct, ", ") + ")")
	}
	return nil
}
