package stored

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/pkg/crypto"
	"github.com/wyolet/relay/secret/aead"
)

// Downgrade rewrites every sealed row back to the legacy shape (AES-GCM under masterKey, the RELAY_MASTER_KEY a previous relay version reads), so secrets written since the upgrade survive a rollback. Run it with every relay server stopped: a running one would seal the rows again. Rows it cannot decrypt are listed in the error and left sealed, which keeps the schema rollback refusing.
func (s *Backend) Downgrade(ctx context.Context, masterKey []byte) (int, error) {
	ring, err := s.keys.load(ctx)
	if err != nil {
		return 0, err
	}
	done := 0
	var failed []string
	err = storage.WithAdvisoryLock(ctx, s.keys.pool, rewriterLock, func(ctx context.Context) error {
		after := ""
		for {
			rows, err := s.q.ListSealedSecretValues(ctx, gen.ListSealedSecretValuesParams{After: after, MaxRows: rewriteBatch})
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, r := range rows {
				after = r.ID
				plain, _, err := openValue(ctx, ring, r.ID, r.Scope.String, r.Ciphertext, nil)
				if err != nil {
					failed = append(failed, fmt.Sprintf("%s: %v", r.ID, err))
					continue
				}
				switch err := s.writeLegacy(ctx, masterKey, r.ID, plain, r.Ciphertext); {
				case err == nil:
					done++
				case errors.Is(err, errValueChanged):
					failed = append(failed, r.ID+": changed while downgrading; run again")
				default:
					return fmt.Errorf("secret: downgrade %q: %w", r.ID, err)
				}
			}
		}
	})
	if err != nil {
		return done, err
	}
	if len(failed) > 0 {
		return done, fmt.Errorf("secret: %d stored secrets left sealed: %v", len(failed), failed)
	}
	return done, nil
}

// SealedRows counts the stored secrets a relay from before the key ring cannot read (sealed, or without a nonce), the same rows migration 34's down refuses; 0 on a schema that predates the key ring.
func (s *Backend) SealedRows(ctx context.Context) (int64, error) {
	n, err := s.q.CountSealedSecretValues(ctx)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42703" {
		return 0, nil
	}
	return n, err
}

func (s *Backend) writeLegacy(ctx context.Context, masterKey []byte, id string, plain, previous []byte) error {
	ct, nonce, err := crypto.Encrypt(masterKey, plain)
	// Legacy ciphertext that happens to start with the sealed magic would read as sealed; a fresh nonce gives other bytes.
	for err == nil && aead.IsSealed(ct) {
		ct, nonce, err = crypto.Encrypt(masterKey, plain)
	}
	if err != nil {
		return err
	}
	tx, err := s.keys.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.q.WithTx(tx)
	if err := q.AllowLegacySecretWrite(ctx); err != nil {
		return err
	}
	n, err := q.ReplaceSecretValueLegacy(ctx, gen.ReplaceSecretValueLegacyParams{ID: id, Ciphertext: ct, Nonce: nonce, Previous: previous})
	if err != nil {
		return writeError(err)
	}
	if n == 0 {
		return errValueChanged
	}
	return tx.Commit(ctx)
}
