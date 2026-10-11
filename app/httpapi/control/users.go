// The account endpoints: GET /users lists them, PUT /users/by-id/{id} edits
// the two fields an operator owns. Users are identity, not a catalog kind, so
// they get no CRUD factory — the list is a projection that deliberately omits
// every credential field (password hash, IdP subject).

package control

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/authz"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/refcheck"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/principal"
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

// userWriter is what the update handler needs: the reads relay's own rules and the response take, and the directory's operations for the write, which bump the token version and end the sessions.
type userWriter interface {
	Get(ctx context.Context, id string) (*user.User, error)
	List(ctx context.Context) ([]*user.User, error)
	UpdateUser(ctx context.Context, p *principal.Principal, u *directory.User) error
	DisableUser(ctx context.Context, id string) error
	EnableUser(ctx context.Context, id string) error
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
	users := directoryUsers{Store: d.Users, ops: newDirectoryOps(d)}
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
		return updateUser(ctx, users, d.Authz, in)
	})
}

func isActiveAdmin(disabled bool, roles []string) bool {
	return !disabled && slices.Contains(roles, user.RoleAdmin)
}

// keepOneAdmin refuses a change that would leave no enabled admin: nobody
// could grant the role back short of editing the database.
func keepOneAdmin(ctx context.Context, users userWriter, changing string) error {
	all, err := users.List(ctx)
	if err != nil {
		return huma.Error500InternalServerError(err.Error())
	}
	for _, other := range all {
		if other.ID != changing && isActiveAdmin(other.Disabled, other.Roles) {
			return nil
		}
	}
	return huma.Error409Conflict("this is the last enabled admin; grant the role to another account first")
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
	wasDisabled, oldRoles := u.Disabled, u.Roles
	disabled, roles := u.Disabled, u.Roles
	if in.Body.Disabled != nil {
		disabled = *in.Body.Disabled
	}
	if in.Body.Roles != nil {
		roles = *in.Body.Roles
	}
	rolesChanged := !slices.Equal(roles, oldRoles)
	// users.update is not a grant to hand out roles, or its holder could
	// make themselves an admin.
	if rolesChanged {
		if err := refcheck.RequireAdmin(ctx, az, "changing roles"); err != nil {
			return nil, huma.Error403Forbidden(err.Error())
		}
	}
	if isActiveAdmin(wasDisabled, oldRoles) && !isActiveAdmin(disabled, roles) {
		if err := keepOneAdmin(ctx, users, u.ID); err != nil {
			return nil, err
		}
	}
	// The directory ends the sessions on either change, since a live session keeps the roles it logged in with, and bumps the token version on a toggle in both directions: the snapshot drops a disabled account's version, and re-enabling must revive no pre-disable token.
	switch {
	case disabled != wasDisabled && !rolesChanged && disabled:
		err = users.DisableUser(ctx, u.ID)
	case disabled != wasDisabled && !rolesChanged:
		err = users.EnableUser(ctx, u.ID)
	default:
		u.Disabled, u.Roles = disabled, roles
		err = users.UpdateUser(ctx, principalOf(ctx, az), u.ToDirectory())
	}
	if err != nil {
		if errors.Is(err, authz.ErrForbidden) || errors.Is(err, authz.ErrUnauthenticated) {
			return nil, mapAuthzErr(err)
		}
		return nil, huma.Error500InternalServerError(err.Error())
	}
	u.Disabled, u.Roles = disabled, roles
	audit.Record(ctx, "users.update", audit.Resource{
		Kind: "user", ID: u.ID, Name: u.Username, Owner: &owner,
	}, audit.StatusAllowed)
	return &userUpdateOutput{Body: userRow{
		ID: u.ID, Username: u.Username, Email: u.Email,
		Roles: u.Roles, Disabled: u.Disabled,
	}}, nil
}
