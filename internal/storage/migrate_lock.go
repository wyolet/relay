package storage

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strconv"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source"
)

// MigrationDSN is dsn with Postgres' lock_timeout set to d, for the connections migrations run on: a migration queued behind a long transaction would otherwise hold every later query on its table behind it. A dsn that sets lock_timeout itself keeps it, and 0 leaves dsn unchanged.
func MigrationDSN(dsn string, d time.Duration) (string, error) {
	if d <= 0 {
		return dsn, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("storage: parse dsn: %w", err)
	}
	q := u.Query()
	if q.Has("lock_timeout") {
		return dsn, nil
	}
	q.Set("lock_timeout", strconv.FormatInt(d.Milliseconds(), 10))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// lockNotAvailable is the SQLSTATE of a statement that gave up waiting for a lock, as lock_timeout ends it.
const lockNotAvailable = "55P03"

// migrationCause is the driver error behind a failed migration. golang-migrate's database.Error does not unwrap, and its message carries the whole migration body.
func migrationCause(err error) error {
	var byValue database.Error
	var byPointer *database.Error
	switch {
	case errors.As(err, &byValue):
		return byValue.OrigErr
	case errors.As(err, &byPointer):
		return byPointer.OrigErr
	}
	return err
}

// lockTimedOut reports whether err is a migration that gave up waiting for a lock.
func lockTimedOut(err error) bool {
	var pg interface{ SQLState() string }
	return errors.As(migrationCause(err), &pg) && pg.SQLState() == lockNotAvailable
}

// migrationFile names the file of migration version in the given direction, or just the version when the source can't read it.
func migrationFile(src source.Driver, version uint, up bool) string {
	read, dir := src.ReadUp, "up"
	if !up {
		read, dir = src.ReadDown, "down"
	}
	r, ident, err := read(version)
	if err != nil {
		return fmt.Sprintf("migration %d", version)
	}
	_ = r.Close()
	return fmt.Sprintf("%06d_%s.%s.sql", version, ident, dir)
}

// retryable turns a migration that timed out waiting for a lock into a clean, retryable failure. A migration file runs as one implicit transaction, so it applied nothing; only the dirty mark golang-migrate set before running it remains, and that refuses every later run. up says which way the failed migration ran.
func retryable(m *migrate.Migrate, src source.Driver, up bool, err error) error {
	if !lockTimedOut(err) {
		return err
	}
	cause := migrationCause(err)
	v, dirty, verr := m.Version()
	if verr != nil || !dirty {
		return fmt.Errorf("%w; it timed out waiting for a lock, retry once the blocking transaction ends", cause)
	}
	// Up marks the failed migration's version dirty, down the one below it; the clean version is the neighbour.
	var clean uint
	var nerr error
	if up {
		clean, nerr = src.Prev(v)
	} else {
		clean, nerr = src.Next(v)
	}
	switch {
	case up:
		cause = fmt.Errorf("%s: %w", migrationFile(src, v, true), cause)
	case nerr == nil:
		cause = fmt.Errorf("%s: %w", migrationFile(src, clean, false), cause)
	}
	switch {
	case up && errors.Is(nerr, fs.ErrNotExist):
		nerr = m.Force(database.NilVersion)
	case nerr == nil:
		nerr = m.Force(int(clean))
	}
	if nerr != nil {
		return fmt.Errorf("%w; it timed out waiting for a lock and the schema is left marked dirty: %v", cause, nerr)
	}
	return fmt.Errorf("%w; it timed out waiting for a lock and applied nothing, retry once the blocking transaction ends", cause)
}
