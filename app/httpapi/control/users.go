// GET /users — the account list the admin UI needs to name subjects when
// authoring RoleBindings. Users are identity, not a catalog kind, so they
// get no CRUD factory: this is a read of a projection that deliberately
// omits every credential field (password hash, IdP subject).
package control

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
)

// userRow is the projection returned by GET /users.
type userRow struct {
	ID       string   `json:"id"`
	Username string   `json:"username"`
	Email    string   `json:"email,omitempty"`
	Roles    []string `json:"roles,omitempty"`
	Disabled bool     `json:"disabled"`
}

type usersListOutput struct {
	Body struct {
		Items []userRow `json:"items"`
		Total int       `json:"total"`
	}
}

func registerUsers(api huma.API, d Deps, protect huma.Middlewares) {
	if d.Users == nil {
		return
	}
	huma.Register(api, huma.Operation{
		OperationID: "list_users",
		Method:      http.MethodGet,
		Path:        "/users",
		Summary:     "List user accounts",
		Description: "Identity rows only: id, username, email, roles, disabled. " +
			"Never returns password hashes or identity-provider subjects.",
		Tags:        []string{"users"},
		Middlewares: protect,
		Errors:      []int{401, 403, 500},
	}, func(ctx context.Context, _ *struct{}) (*usersListOutput, error) {
		// The global scope, not "any scope": the list is every account in
		// the deployment, so a binding inside one team or project is not a
		// grant to enumerate it.
		owner := meta.Owner{Kind: meta.OwnerSystem}
		if err := d.Authz.Authorize(ctx, "users.list", authz.Resource{Kind: "user", Owner: &owner}); err != nil {
			return nil, mapAuthzErr(err)
		}
		all, err := d.Users.List(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &usersListOutput{}
		out.Body.Items = make([]userRow, 0, len(all))
		for _, u := range all {
			out.Body.Items = append(out.Body.Items, userRow{
				ID: u.ID, Username: u.Username, Email: u.Email,
				Roles: u.Roles, Disabled: u.Disabled,
			})
		}
		out.Body.Total = len(out.Body.Items)
		return out, nil
	})
}
