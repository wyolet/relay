// The account endpoints: GET /users lists them, PUT /users/by-id/{id} edits
// the two fields an operator owns. Users are identity, not a catalog kind, so
// they get no CRUD factory — the list is a projection that deliberately omits
// every credential field (password hash, IdP subject).
package control

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
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
	registerUserUpdate(api, d, protect)
}

// userWriter is the narrow store surface the update handler needs.
// *user.Store satisfies it; tests supply a fake.
type userWriter interface {
	Get(ctx context.Context, id string) (*user.User, error)
	Upsert(ctx context.Context, u *user.User) error
	BumpTokenVersion(ctx context.Context, id string) error
}

type userUpdateInput struct {
	ID   string `path:"id"`
	Body struct {
		Disabled *bool     `json:"disabled,omitempty" doc:"Disable or re-enable the account. Disabling also invalidates every inference token the user holds."`
		Roles    *[]string `json:"roles,omitempty" doc:"Replace the account's roles. Omit to leave them unchanged."`
	}
}

type userUpdateOutput struct {
	Body userRow
}

func registerUserUpdate(api huma.API, d Deps, protect huma.Middlewares) {
	huma.Register(api, huma.Operation{
		OperationID: "update_user",
		Method:      http.MethodPut,
		Path:        "/users/by-id/{id}",
		Summary:     "Update a user account",
		Description: "Edits `disabled` and `roles`. Disabling bumps the user's " +
			"token version, so tokens already minted stop verifying.",
		Tags:        []string{"users"},
		Middlewares: protect,
		Errors:      []int{401, 403, 404, 500},
	}, func(ctx context.Context, in *userUpdateInput) (*userUpdateOutput, error) {
		return updateUser(ctx, d.Users, d.Authz, in)
	})
}

// updateUser applies the two editable fields. Authorization is at the global
// scope, like the list: an account belongs to the deployment, not to a team,
// so a binding inside one project is not a grant to edit anyone.
func updateUser(ctx context.Context, users userWriter, az authz.Authorizer, in *userUpdateInput) (*userUpdateOutput, error) {
	owner := meta.Owner{Kind: meta.OwnerSystem}
	if err := az.Authorize(ctx, "users.update", authz.Resource{Kind: "user", ID: in.ID, Owner: &owner}); err != nil {
		return nil, mapAuthzErr(err)
	}
	u, err := users.Get(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	if u == nil {
		return nil, huma.Error404NotFound("user not found")
	}
	wasDisabled := u.Disabled
	if in.Body.Disabled != nil {
		u.Disabled = *in.Body.Disabled
	}
	if in.Body.Roles != nil {
		u.Roles = *in.Body.Roles
	}
	if err := users.Upsert(ctx, u); err != nil {
		return nil, huma.Error500InternalServerError(err.Error())
	}
	// The snapshot already drops a disabled account's token version, but a
	// later re-enable would revive every token minted before the disable.
	if u.Disabled && !wasDisabled {
		if err := users.BumpTokenVersion(ctx, in.ID); err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
	}
	audit.Record(ctx, "users.update", audit.Resource{
		Kind: "user", ID: u.ID, Name: u.Username, Owner: &owner,
	}, audit.StatusAllowed)
	return &userUpdateOutput{Body: userRow{
		ID: u.ID, Username: u.Username, Email: u.Email,
		Roles: u.Roles, Disabled: u.Disabled,
	}}, nil
}
