package storage

import (
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/wyolet/relay/internal/catalog"
)

// translateCatalogErr maps low-level pgx errors to domain sentinel errors.
// Unknown errors are returned as-is.
func translateCatalogErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return catalog.ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			return catalog.ErrConflict
		case "23503": // foreign_key_violation
			return catalog.ErrInvalidSpec
		}
	}
	return err
}
