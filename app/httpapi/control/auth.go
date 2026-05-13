package control

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"github.com/wyolet/relay/app/actor"
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
		UserID   string `json:"user_id"`
		Username string `json:"username"`
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
		if d.Identity == nil {
			return nil, huma.Error503ServiceUnavailable("login not configured (no identity store)")
		}
		user, ok := d.Identity.ByUsername(in.Body.Username)
		if !ok {
			return nil, huma.Error401Unauthorized("invalid credentials")
		}
		if !identity.Verify(user, in.Body.Password) {
			return nil, huma.Error401Unauthorized("invalid credentials")
		}
		if err := d.Sessions.Login(ctx, user.Metadata.Name, user.Spec.Username.Get()); err != nil {
			return nil, huma.Error500InternalServerError("session create failed: " + err.Error())
		}
		out := &authResponse{}
		out.Body.UserID = user.Metadata.Name
		out.Body.Username = user.Spec.Username.Get()
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
		return out, nil
	})
}

// Deps placeholder — actual struct defined in control.go; this file just
// uses fields. Provided here as a compile-time guard against renames.
var _ = func() Deps { return Deps{} }
