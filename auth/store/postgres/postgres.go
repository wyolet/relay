// Package postgres is the ready-made Postgres implementation of directory.Stores: the tables, the queries, and the embedded migrations a product runs with Migrate under a version table of the module's own.
//
// The JSONB documents it writes keep a fixed shape (see jsonb.go) so a product that already holds these tables reads and writes the same rows. Out of scope: connection pooling and transactions across stores, which are the caller's (pass a pgx.Tx to New to share one), and any table the directory does not own.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/wyolet/relay/auth/directory"
	"github.com/wyolet/relay/auth/store/postgres/internal/gen"
)

// DB is what the stores run statements on: a *pgxpool.Pool, a *pgx.Conn or a pgx.Tx. Begin on a transaction opens a savepoint, so a store whose write spans tables works unchanged inside the caller's transaction.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// New returns the stores over db.
func New(db DB) directory.Stores {
	return directory.Stores{
		Users:           users{db},
		Groups:          groups{db},
		Teams:           teams{db},
		Projects:        projects{db},
		ServiceAccounts: serviceAccounts{db},
		Roles:           roles{db},
		RoleBindings:    bindings{db},
	}
}

// inTx runs fn in a transaction on db and commits when it returns nil.
func inTx(ctx context.Context, db DB, fn func(q *gen.Queries) error) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if err := fn(gen.New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// text maps "" to NULL so the unique columns never collide on empty strings.
func text(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// expectedVersion turns Meta.ResourceVersion into the conditional-upsert parameter.
func expectedVersion(m directory.Metadata) (pgtype.Int8, error) {
	v, ok, err := m.ExpectedVersion()
	return pgtype.Int8{Int64: v, Valid: ok}, err
}

// staleIfNoRows reads a conditional upsert that touched no row as a version mismatch.
func staleIfNoRows(n int64, err error) error {
	if err != nil {
		return err
	}
	if n == 0 {
		return directory.ErrStaleResourceVersion
	}
	return nil
}

// mapErr turns a constraint violation into the directory's error, wrapping the driver's for detail.
func mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return fmt.Errorf("%s: %w: %s", op, directory.ErrConflict, pgErr.ConstraintName)
		case "23503":
			return fmt.Errorf("%s: %w: %s", op, directory.ErrInvalid, pgErr.Detail)
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// notFound reports a Get that matched nothing, so callers return (nil, nil).
func notFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
