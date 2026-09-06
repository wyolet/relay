package control

import (
	"strconv"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"

	"github.com/wyolet/relay/app/authz"
)

// The generated OpenAPI is what a client codegen believes: a status the
// handler can return but the operation does not declare is a response the
// client has no type for.
func TestTokenOperationsDeclareTheirErrorStatuses(t *testing.T) {
	api := humachi.New(chi.NewRouter(), huma.DefaultConfig("token-errors-test", "0"))
	registerTokens(api, Deps{Authz: authz.AlwaysAllowAuthenticated{}}, nil)
	paths := api.OpenAPI().Paths

	for _, tc := range []struct {
		path  string
		codes []int
	}{
		{"/auth/token", []int{429, 500}},
		{"/auth/token/revoke", []int{400, 500}},
	} {
		item := paths[tc.path]
		if item == nil || item.Post == nil {
			t.Fatalf("no POST operation at %s", tc.path)
		}
		for _, code := range tc.codes {
			if _, ok := item.Post.Responses[strconv.Itoa(code)]; !ok {
				t.Errorf("POST %s does not declare %d", tc.path, code)
			}
		}
	}
}
