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
	"github.com/wyolet/relay/pkg/ids"
)

// userRow is the projection returned by GET /users.
type userRow struct {
	ID       string   `json:"id"              doc:"User id."`
	Username string   `json:"username"        doc:"Sign-in name."`
	Email    string   `json:"email,omitempty" doc:"Email address, when known."`
	Roles    []string `json:"roles,omitempty" doc:"Names of the roles bound to the user."`
	Disabled bool     `json:"disabled"        doc:"A disabled user cannot sign in and its tokens and keys stop authenticating."`
	// PersonalAccount is the user's own value; PersonalAccountEffective what the plans apply.
	PersonalAccount          string                    `json:"personalAccount,omitempty" enum:"on,off" doc:"The user's own say on their personal account: on or off. Omitted leaves it to their groups and the accounts settings section."`
	PersonalAccountEffective *PersonalAccountEffective `json:"personalAccountEffective,omitempty" readOnly:"true" doc:"Whether the user's personal account serves, and what decided it. Omitted until the plans are loaded."`
}

// userRowOf projects u for a read, with its personal-account state from the plans.
func userRowOf(d Deps, u *user.User) userRow {
	return userRow{
		ID: u.ID, Username: u.Username, Email: u.Email, Roles: u.Roles, Disabled: u.Disabled,
		PersonalAccount: u.PersonalAccount, PersonalAccountEffective: personalAccountEffective(d, u.ID),
	}
}

type usersListOutput struct {
	Body struct {
		Items []userRow `json:"items" doc:"The page of users."`
		Total int       `json:"total" doc:"Users matching before limit and offset apply."`
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
		Description: "Identity rows only: id, username, email, roles, disabled, and the personal-account switch (personalAccount, the user's own value, and personalAccountEffective, what applies and why). " +
			"Never returns password hashes or identity-provider subjects.",
		Tags:        []string{"users"},
		Middlewares: protect,
		Errors:      []int{401, 403, 500},
	}, func(ctx context.Context, _ *struct{}) (*usersListOutput, error) {
		// The global scope, not "any scope": the list is every account in
		// the deployment, so a binding inside one team or project is not a
		// grant to enumerate it.
		if err := d.Authz.Authorize(ctx, "users.list", userResource("")); err != nil {
			return nil, mapAuthzErr(err)
		}
		all, err := d.Users.List(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		out := &usersListOutput{}
		out.Body.Items = make([]userRow, 0, len(all))
		for _, u := range all {
			out.Body.Items = append(out.Body.Items, userRowOf(d, u))
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
	SetPersonalAccount(ctx context.Context, id, value string) (bool, error)
}

type userUpdateInput struct {
	ID   string `path:"id" doc:"User id."`
	Body struct {
		Disabled        *bool     `json:"disabled,omitempty" doc:"Disable or re-enable the account. Disabling also invalidates every inference token the user holds."`
		Roles           *[]string `json:"roles,omitempty" doc:"Replace the account's roles. Omit to leave them unchanged."`
		PersonalAccount *string   `json:"personalAccount,omitempty" enum:"on,off," doc:"Turn the user's personal account on or off for them alone, over their groups and the accounts settings section; an empty string leaves it to those again. Omit to leave it unchanged. Admin only, never the user themselves (403)."`
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
		Description: "Edits `disabled`, `roles` and `personalAccount`. Disabling bumps the user's " +
			"token version, so tokens already minted stop verifying. Changing roles or personalAccount takes an admin. " +
			"personalAccountEffective in the answer is read from the plans and may lag the write by about a second.",
		Tags:        []string{"users"},
		Middlewares: protect,
		Errors:      []int{400, 401, 403, 404, 500},
	}, func(ctx context.Context, in *userUpdateInput) (*userUpdateOutput, error) {
		out, err := updateUser(ctx, users, d.Authz, in)
		if out != nil {
			out.Body.PersonalAccountEffective = personalAccountEffective(d, out.Body.ID)
		}
		return out, err
	})
}

// userResource is the resource every account route authorizes: an account belongs to the deployment, so it lives in the global scope. id is empty for the list.
func userResource(id string) authz.Resource {
	return authz.Resource{Kind: "user", ID: id, Owner: &meta.Owner{Kind: meta.OwnerSystem}}
}

// userFinder reads one account by id or by username.
type userFinder interface {
	Get(ctx context.Context, id string) (*user.User, error)
	ByUsername(ctx context.Context, username string) (*user.User, error)
}

// findUser resolves ref, an id or a username, to its account.
func findUser(ctx context.Context, users userFinder, ref string) (*user.User, error) {
	get := users.ByUsername
	if ids.Valid(ref) {
		get = users.Get
	}
	u, err := get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, errSlugNotFound
	}
	return u, nil
}

// userRows finds accounts the way the account routes authorize them; the list is at the global scope too.
func userRows(users userFinder) kindRows {
	return kindRows{
		plural: "users", singular: "user", listOwner: userResource("").Owner,
		find: func(ctx context.Context, ref string) (authz.Resource, error) {
			u, err := findUser(ctx, users, ref)
			if err != nil {
				return authz.Resource{}, err
			}
			res := userResource(u.ID)
			res.Name = u.Username
			return res, nil
		},
	}
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

// updateUser applies the editable fields. Authorization is at the global
// scope, like the list: an account belongs to the deployment, not to a team,
// so a binding inside one project is not a grant to edit anyone.
func updateUser(ctx context.Context, users userWriter, az authz.Authorizer, in *userUpdateInput) (*userUpdateOutput, error) {
	personal := in.Body.PersonalAccount
	if personal != nil {
		switch *personal {
		case "", "on", "off":
		default:
			return nil, huma.Error400BadRequest(`personalAccount must be "on", "off", or "" to leave it to the user's groups and the deployment`)
		}
	}
	res := userResource(in.ID)
	if err := az.Authorize(ctx, "users.update", res); err != nil {
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
	personalChanged := personal != nil && *personal != u.PersonalAccount
	// A user's personalAccount decides whether they may act on their own, so it is never theirs to change.
	if personalChanged {
		if err := refcheck.RequireAdmin(ctx, az, "a user's personalAccount"); err != nil {
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
	case personal != nil && disabled == wasDisabled && !rolesChanged:
		// Only the personal account is asked for: the directory row is left alone.
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
		if errors.Is(err, directory.ErrStaleResourceVersion) {
			return nil, staleVersionError("user", u.ID)
		}
		return nil, huma.Error500InternalServerError(err.Error())
	}
	// After the directory write: this one bumps the row's version, which that write is conditional on.
	if personalChanged {
		found, err := users.SetPersonalAccount(ctx, u.ID, *personal)
		if err != nil {
			return nil, huma.Error500InternalServerError(err.Error())
		}
		if !found {
			return nil, huma.Error404NotFound("user not found")
		}
		u.PersonalAccount = *personal
		audit.Changed(ctx, []string{"personalAccount"})
	}
	u.Disabled, u.Roles = disabled, roles
	audit.Record(ctx, "users.update", audit.Resource{
		Kind: "user", ID: u.ID, Name: u.Username, Owner: res.Owner,
	}, audit.StatusAllowed)
	return &userUpdateOutput{Body: userRow{
		ID: u.ID, Username: u.Username, Email: u.Email,
		Roles: u.Roles, Disabled: u.Disabled, PersonalAccount: u.PersonalAccount,
	}}, nil
}
