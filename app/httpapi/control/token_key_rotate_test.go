package control

import (
	"context"
	"errors"
	"testing"

	"github.com/wyolet/relay/app/actor"
	"github.com/wyolet/relay/app/authz"
)

func rotateCtx() context.Context {
	return actor.WithActor(context.Background(), &actor.Actor{AdminToken: true, Username: "admin-token"})
}

func TestRotateTokenKeyRunsTheWiredRotation(t *testing.T) {
	called := 0
	d := Deps{
		Authz:          authz.AlwaysAllowAuthenticated{},
		RotateTokenKey: func(context.Context) error { called++; return nil },
	}
	if _, err := rotateTokenKey(rotateCtx(), d); err != nil {
		t.Fatalf("rotateTokenKey: %v", err)
	}
	if called != 1 {
		t.Fatalf("rotation ran %d times, want 1", called)
	}
}

func TestRotateTokenKeyIsAuthorized(t *testing.T) {
	d := Deps{Authz: denyAll{}, RotateTokenKey: func(context.Context) error {
		t.Fatal("rotation ran for a denied caller")
		return nil
	}}
	_, err := rotateTokenKey(rotateCtx(), d)
	if got := statusOf(t, err); got != 403 {
		t.Fatalf("status = %d, want 403", got)
	}
}

// A deployment with no secret store cannot keep a new key, so the endpoint
// says so rather than pretending the rotation happened.
func TestRotateTokenKeyWithoutAStoreIs503(t *testing.T) {
	d := Deps{Authz: authz.AlwaysAllowAuthenticated{}}
	_, err := rotateTokenKey(rotateCtx(), d)
	if got := statusOf(t, err); got != 503 {
		t.Fatalf("status = %d, want 503", got)
	}
}

func TestRotateTokenKeyReportsAFailedRotation(t *testing.T) {
	d := Deps{
		Authz:          authz.AlwaysAllowAuthenticated{},
		RotateTokenKey: func(context.Context) error { return errors.New("no master key") },
	}
	_, err := rotateTokenKey(rotateCtx(), d)
	if got := statusOf(t, err); got != 500 {
		t.Fatalf("status = %d, want 500", got)
	}
}
