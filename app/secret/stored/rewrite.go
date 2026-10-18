package stored

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/internal/storage"
	"github.com/wyolet/relay/internal/storage/gen"
	"github.com/wyolet/relay/secret/keyring"
)

// rewriterLock is the advisory-lock id that keeps the rewriter to one process at a time.
const rewriterLock int64 = 0x52454C41595F5357

const rewriteBatch = 100

// errValueChanged reports a row whose value changed since the rewriter read it; the newer write wins.
var errValueChanged = errors.New("secret: stored value changed during rewrite")

// RewriteResult counts what a rewrite pass did. Failed rows could not be decrypted here (a KEK this process lacks, or tampered bytes) and stay as they are.
type RewriteResult struct {
	Rewritten int
	Changed   int
	Failed    int
}

// RewriteStale moves every stale row (legacy shape, or sealed under a key that is no longer its scope's Primary) under its scope's Primary, waiting for a running pass elsewhere to finish first. It is the synchronous form of the background rewriter, for running with the servers stopped.
func (s *Backend) RewriteStale(ctx context.Context) (RewriteResult, error) {
	ring, err := s.keys.load(ctx)
	if err != nil {
		return RewriteResult{}, err
	}
	var res RewriteResult
	err = storage.WithAdvisoryLock(ctx, s.keys.pool, rewriterLock, func(ctx context.Context) error {
		res, err = s.rewritePass(ctx, ring)
		return err
	})
	return res, err
}

// runRewriter runs a pass whenever it is woken, on the one process that wins the advisory lock; the others skip, since the pass reads every stale row from the database anyway. A failed pass is retried after a minute.
func (s *Backend) runRewriter(ctx context.Context, ring *keyring.Ring) {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.keys.wake:
		case <-retry:
		}
		retry = nil
		var res RewriteResult
		ran, err := storage.TryAdvisoryLock(ctx, s.keys.pool, rewriterLock, func(ctx context.Context) error {
			var err error
			res, err = s.rewritePass(ctx, ring)
			return err
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			slog.Error("secret: rewriting stale stored secrets failed; retrying", "err", err)
			retry = time.After(time.Minute)
		}
		if ran && res != (RewriteResult{}) {
			slog.Info("secret: rewrote stale stored secrets", "rewritten", res.Rewritten, "changed", res.Changed, "failed", res.Failed)
			s.warnLegacy(ctx)
		}
	}
}

func (s *Backend) rewritePass(ctx context.Context, ring *keyring.Ring) (RewriteResult, error) {
	var res RewriteResult
	after := ""
	for {
		rows, err := s.q.ListStaleSecretValues(ctx, gen.ListStaleSecretValuesParams{After: after, MaxRows: rewriteBatch})
		if err != nil {
			return res, err
		}
		if len(rows) == 0 {
			return res, nil
		}
		for _, r := range rows {
			after = r.ID
			switch err := s.rewriteRow(ctx, ring, r); {
			case err == nil:
				res.Rewritten++
			case errors.Is(err, errValueChanged):
				res.Changed++
			case ctx.Err() != nil:
				return res, ctx.Err()
			default:
				res.Failed++
				slog.Warn("secret: cannot rewrite stored secret", "id", r.ID, "err", err)
			}
		}
	}
}

func (s *Backend) rewriteRow(ctx context.Context, ring *keyring.Ring, r gen.ListStaleSecretValuesRow) error {
	scope, owned := rowScope(r.Scope, r.HasOwner, r.OwnerKind, r.OwnerID)
	plain, _, err := openValue(ctx, ring, r.ID, scope, r.Ciphertext, r.Nonce)
	if err != nil {
		return err
	}
	if !owned {
		slog.Info("secret: stored secret has no owning host key; sealing it under the system scope", "id", r.ID)
	}
	return ring.EncryptAndWrite(ctx, scope, plain, valueAAD(r.ID), func(sealed []byte, keyID [16]byte) error {
		n, err := s.q.ReplaceSecretValue(ctx, gen.ReplaceSecretValueParams{
			ID: r.ID, Ciphertext: sealed, Scope: pgtype.Text{String: scope, Valid: true}, DekID: uuid(keyID), Previous: r.Ciphertext,
		})
		if err != nil {
			return writeError(err)
		}
		if n == 0 {
			return errValueChanged
		}
		return nil
	})
}
