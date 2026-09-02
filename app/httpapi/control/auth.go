package control

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/audit"
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
	}
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
		if err := d.Sessions.Login(ctx, yu.Metadata.Name, yu.Spec.Username.Get(), yu.Spec.Roles...); err != nil {
			return nil, huma.Error500InternalServerError("session create failed: " + err.Error())
		}
		audit.Record(ctx, "auth.login", audit.Resource{Kind: "user", ID: yu.Metadata.Name, Name: yu.Spec.Username.Get()}, audit.StatusAllowed, audit.Actor{Kind: audit.ActorUser, ID: yu.Metadata.Name, Name: yu.Spec.Username.Get()})
		out := &authResponse{}
		out.Body.UserID = yu.Metadata.Name
		out.Body.Username = yu.Spec.Username.Get()
		out.Body.Roles = yu.Spec.Roles
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
		return out, nil
	})
}

// Deps placeholder — actual struct defined in control.go; this file just
// uses fields. Provided here as a compile-time guard against renames.
var _ = func() Deps { return Deps{} }
