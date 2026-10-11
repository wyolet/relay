//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wyolet/relay/auth/apikey"
	"github.com/wyolet/relay/auth/scim"
)

func TestSCIMTokens(t *testing.T) {
	ctx := context.Background()
	s := NewSCIMTokens(openPool(t, migratedDB(t)))

	token, c, err := scim.CreateClient(ctx, s, "entra")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ClientByTokenHash(ctx, apikey.Hash(token))
	if err != nil || got == nil || got.Name != "entra" || got.Revoked || got.TokenPrefix != c.TokenPrefix || got.CreatedAt.IsZero() {
		t.Fatalf("lookup: %+v, %v", got, err)
	}
	if miss, err := s.ClientByTokenHash(ctx, apikey.Hash(token+"x")); miss != nil || err != nil {
		t.Fatalf("lookup of another token: %+v, %v", miss, err)
	}
	if _, _, err := scim.CreateClient(ctx, s, "entra"); !errors.Is(err, scim.ErrClientExists) {
		t.Fatalf("second create of a live name: %v", err)
	}

	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.MarkUsed(ctx, "entra", at); err != nil {
		t.Fatal(err)
	}
	// A second use within the minute is not written.
	if err := s.MarkUsed(ctx, "entra", at.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListClients(ctx)
	if err != nil || len(list) != 1 || !list[0].LastUsedAt.Equal(at) {
		t.Fatalf("list: %+v, %v", list, err)
	}

	// Rotation swaps the token in place: the old one finds nothing, the new one finds the same live client, unused so far.
	rotated, _, err := scim.RotateClient(ctx, s, "entra")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if old, _ := s.ClientByTokenHash(ctx, apikey.Hash(token)); old != nil {
		t.Fatalf("the old token still finds %+v", old)
	}
	if got, _ := s.ClientByTokenHash(ctx, apikey.Hash(rotated)); got == nil || got.Name != "entra" || got.Revoked || !got.LastUsedAt.IsZero() {
		t.Fatalf("rotated client reads as %+v", got)
	}
	if _, _, err := scim.RotateClient(ctx, s, "nobody"); !errors.Is(err, scim.ErrNoClient) {
		t.Fatalf("rotating a missing client: %v", err)
	}
	token = rotated

	if err := s.RevokeClient(ctx, "entra"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := scim.RotateClient(ctx, s, "entra"); !errors.Is(err, scim.ErrClientRevoked) {
		t.Fatalf("rotating a revoked client: %v", err)
	}
	if err := s.RevokeClient(ctx, "entra"); err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
	if err := s.RevokeClient(ctx, "nobody"); !errors.Is(err, scim.ErrNoClient) {
		t.Fatalf("revoking a missing client: %v", err)
	}
	if got, _ := s.ClientByTokenHash(ctx, apikey.Hash(token)); got == nil || !got.Revoked {
		t.Fatalf("revoked client reads as %+v", got)
	}

	// A revoked name is not issued again, so no new token inherits what the old one provisioned; the old token stays dead.
	if _, _, err := scim.CreateClient(ctx, s, "entra"); !errors.Is(err, scim.ErrClientExists) {
		t.Fatalf("reissuing a revoked name: %v, want ErrClientExists", err)
	}
	if old, _ := s.ClientByTokenHash(ctx, apikey.Hash(token)); old == nil || !old.Revoked {
		t.Fatalf("the old token reads as %+v, want revoked", old)
	}
}
