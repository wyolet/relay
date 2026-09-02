package control

import (
	"context"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
	"github.com/wyolet/relay/app/meta"
	"github.com/wyolet/relay/app/user"
	"github.com/wyolet/relay/internal/identity"
)

// loginInput is the typed body for /auth/login.
type loginInput struct {
	Body struct {
		Username string `json:"username" minLength:"1" doc:"Username (matches identity YAML spec.username)."`
		Password string `json:"password" minLength:"1" doc:"Cleartext password."`
	}
}

// authResponse is shared by /auth/login and /auth/whoami.
type authResponse struct {
	Body struct {
		UserID   string   `json:"user_id"`
		Username string   `json:"username"`
		Roles    []string `json:"roles,omitempty"`
		Subjects []string `json:"subjects,omitempty" doc:"RBAC subject strings this caller acts under."`
		Scopes   []string `json:"scopes,omitempty"   doc:"Scopes the caller holds a role binding at, as \"team:<id>\" / \"project:<id>\"."`
	}
}

// bindingScopes lists the non-global scopes the actor holds any RoleBinding
// at, deduplicated and sorted. Used by the UI to decide which tenancy views
// to offer; it is not an authorization decision.
func bindingScopes(d Deps, a *actor.Actor) []string {
	if d.Catalog == nil || len(a.Subjects) == 0 {
		return nil
	}
	snap := d.Catalog.Current()
	if snap == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, subj := range a.Subjects {
		for _, b := range snap.RoleBindingsForSubject(subj) {
			if b.Spec.Scope.Kind == meta.OwnerSystem || b.Spec.Scope.ID == "" {
				continue
			}
			s := string(b.Spec.Scope.Kind) + ":" + b.Spec.Scope.ID
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

type emptyOutput struct{}

func registerAuth(api huma.API, d Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "auth_login",
		Method:      "POST",
		Path:        "/auth/login",
		Summary:     "Exchange username + password for a session cookie",
		Tags:        []string{"auth"},
		Errors:      []int{401, 503},
	}, func(ctx context.Context, in *loginInput) (*authResponse, error) {
		// DB-backed users first; YAML identity remains the bootstrap /
		// break-glass fallback. Both mint the same session shape.
		if d.Users != nil {
			u, err := d.Users.ByUsername(ctx, in.Body.Username)
			if err != nil {
				return nil, huma.Error500InternalServerError("user lookup failed: " + err.Error())
			}
			if u != nil && !u.Disabled && user.VerifyPassword(u.PasswordHash, in.Body.Password) {
				if err := d.Sessions.Login(ctx, u.ID, u.Username, u.Roles...); err != nil {
					return nil, huma.Error500InternalServerError("session create failed: " + err.Error())
				}
				audit.Record(ctx, "auth.login", audit.Resource{Kind: "user", ID: u.ID, Name: u.Username}, audit.StatusAllowed, audit.Actor{Kind: audit.ActorUser, ID: u.ID, Name: u.Username})
				out := &authResponse{}
				out.Body.UserID = u.ID
				out.Body.Username = u.Username
				out.Body.Roles = u.Roles
				return out, nil
			}
		}
		if d.Identity == nil {
			audit.Record(ctx, "auth.login", audit.Resource{Kind: "user", Name: in.Body.Username}, audit.StatusDenied, audit.Actor{Kind: audit.ActorAnonymous, Name: in.Body.Username})
			return nil, huma.Error401Unauthorized("invalid credentials")
		}
		yu, ok := d.Identity.ByUsername(in.Body.Username)
		if !ok {
			audit.Record(ctx, "auth.login", audit.Resource{Kind: "user", Name: in.Body.Username}, audit.StatusDenied, audit.Actor{Kind: audit.ActorAnonymous, Name: in.Body.Username})
			return nil, huma.Error401Unauthorized("invalid credentials")
		}
		if !identity.Verify(yu, in.Body.Password) {
			audit.Record(ctx, "auth.login", audit.Resource{Kind: "user", Name: in.Body.Username}, audit.StatusDenied, audit.Actor{Kind: audit.ActorAnonymous, Name: in.Body.Username})
			return nil, huma.Error401Unauthorized("invalid credentials")
		}
		// The YAML users are seeded into the table at boot, so the row is
		// what the session must carry: everything downstream (owner ids,
		// key principals, subjects) keys on the UUID, and the YAML slug is
		// not one. Only a deployment with no user store falls back to it.
		username, roles := yu.Spec.Username.Get(), yu.Spec.Roles
		userID := yu.Metadata.Name
		if d.Users != nil {
			row, err := d.Users.ByUsername(ctx, username)
			if err != nil {
				return nil, huma.Error500InternalServerError("user lookup failed: " + err.Error())
			}
			if row != nil {
				userID, roles = row.ID, row.Roles
			}
		}
		if err := d.Sessions.Login(ctx, userID, username, roles...); err != nil {
			return nil, huma.Error500InternalServerError("session create failed: " + err.Error())
		}
		audit.Record(ctx, "auth.login", audit.Resource{Kind: "user", ID: userID, Name: username}, audit.StatusAllowed, audit.Actor{Kind: audit.ActorUser, ID: userID, Name: username})
		out := &authResponse{}
		out.Body.UserID = userID
		out.Body.Username = username
		out.Body.Roles = roles
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth_logout",
		Method:      "POST",
		Path:        "/auth/logout",
		Summary:     "Destroy the current session",
		Tags:        []string{"auth"},
		Errors:      []int{},
	}, func(ctx context.Context, _ *struct{}) (*emptyOutput, error) {
		// Logout is intentionally idempotent: no error if no session.
		_ = d.Sessions.Logout(ctx)
		audit.Record(ctx, "auth.logout", audit.Resource{Kind: "user"}, audit.StatusAllowed)
		return &emptyOutput{}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth_whoami",
		Method:      "GET",
		Path:        "/auth/whoami",
		Summary:     "Return the authenticated caller, if any",
		Tags:        []string{"auth"},
		Errors:      []int{401},
	}, func(ctx context.Context, _ *struct{}) (*authResponse, error) {
		a := actor.From(ctx)
		if !a.IsAuthenticated() {
			return nil, huma.Error401Unauthorized("not authenticated")
		}
		out := &authResponse{}
		out.Body.UserID = a.UserID
		out.Body.Username = a.Username
		out.Body.Roles = a.Roles
		out.Body.Subjects = a.Subjects
		out.Body.Scopes = bindingScopes(d, a)
		return out, nil
	})
}

// Deps placeholder — actual struct defined in control.go; this file just
// uses fields. Provided here as a compile-time guard against renames.
var _ = func() Deps { return Deps{} }
