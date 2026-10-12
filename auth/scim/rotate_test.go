package scim

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Rotating a client's token kills the old one at once and leaves the client's name, and so every row it provisioned, reachable with the new one. A revoked client is not rotated back to life.
func TestRotateClientReplacesTheToken(t *testing.T) {
	f := newFixture(t)
	id := f.createUser("ada@example.com", "obj-1")
	old := f.tokenOf["entra"]

	token, c, err := RotateClient(context.Background(), f.tokens, "entra")
	if err != nil {
		t.Fatal(err)
	}
	if token == old || c.Name != "entra" || c.TokenPrefix == "" {
		t.Fatalf("rotated client %+v, token changed: %v", c, token != old)
	}
	f.do(http.MethodGet, "/Users/"+id, "").want(t, http.StatusUnauthorized)
	f.tokenOf["entra"] = token
	if got := f.do(http.MethodGet, "/Users/"+id, "").want(t, http.StatusOK); got.str("id") != id {
		t.Fatalf("the rotated token reads %s", got.raw)
	}

	if _, _, err := RotateClient(context.Background(), f.tokens, "nobody"); !errors.Is(err, ErrNoClient) {
		t.Fatalf("rotating a missing client: %v", err)
	}
	if err := f.tokens.RevokeClient(context.Background(), "entra"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RotateClient(context.Background(), f.tokens, "entra"); !errors.Is(err, ErrClientRevoked) {
		t.Fatalf("rotating a revoked client: %v", err)
	}
	f.do(http.MethodGet, "/Users/"+id, "").want(t, http.StatusUnauthorized)
}
