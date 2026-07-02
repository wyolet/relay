package user

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/wyolet/relay/internal/identity"
	"github.com/wyolet/relay/pkg/ids"
)

// SeedFromIdentity copies YAML identity users (config/users/) into the users
// table, seed-if-absent by username — same pattern as the settings seed. The
// YAML files remain the bootstrap/break-glass path; the table is what login
// reads. Plain-text YAML passwords are bcrypt-hashed on ingest so cleartext
// never lands in Postgres; already-hashed values are stored verbatim.
func SeedFromIdentity(ctx context.Context, s *Store, id *identity.Store, log *slog.Logger) error {
	if s == nil || id == nil {
		return nil
	}
	seeded := 0
	for _, yu := range id.Users() {
		username := yu.Spec.Username.Get()
		if username == "" {
			continue
		}
		existing, err := s.ByUsername(ctx, username)
		if err != nil {
			return fmt.Errorf("user seed: lookup %q: %w", username, err)
		}
		if existing != nil {
			continue
		}
		hash := yu.Spec.Password.Get()
		if hash != "" && !isBcrypt(hash) {
			hash, err = HashPassword(hash)
			if err != nil {
				return fmt.Errorf("user seed: hash %q: %w", username, err)
			}
		}
		u := &User{
			ID:           ids.New(),
			Username:     username,
			Email:        yu.Spec.Email.Get(),
			PasswordHash: hash,
			Roles:        yu.Spec.Roles,
		}
		if err := s.Upsert(ctx, u); err != nil {
			return fmt.Errorf("user seed: upsert %q: %w", username, err)
		}
		seeded++
	}
	if seeded > 0 {
		log.Info("users seeded from identity YAML", "count", seeded)
	}
	return nil
}
