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

// lockTimedOut reports whether err is a migration that gave up waiting for a lock. golang-migrate's database.Error does not unwrap, so its OrigErr is read directly.
func lockTimedOut(err error) bool {
	var byValue database.Error
	var byPointer *database.Error
	switch {
	case errors.As(err, &byValue):
		err = byValue.OrigErr
	case errors.As(err, &byPointer):
		err = byPointer.OrigErr
	}
	var pg interface{ SQLState() string }
	return errors.As(err, &pg) && pg.SQLState() == lockNotAvailable
}

// retryable turns a migration that timed out waiting for a lock into a clean, retryable failure. A migration file runs as one implicit transaction, so it applied nothing; only the dirty mark golang-migrate set before running it remains, and that refuses every later run. up says which way the failed migration ran.
func retryable(m *migrate.Migrate, src source.Driver, up bool, err error) error {
	if !lockTimedOut(err) {
		return err
	}
	v, dirty, verr := m.Version()
	if verr != nil || !dirty {
		return fmt.Errorf("%w; it timed out waiting for a lock, retry once the blocking transaction ends", err)
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
	case up && errors.Is(nerr, fs.ErrNotExist):
		nerr = m.Force(database.NilVersion)
	case nerr == nil:
		nerr = m.Force(int(clean))
	}
	if nerr != nil {
		return fmt.Errorf("%w; it timed out waiting for a lock and the schema is left marked dirty: %v", err, nerr)
	}
	return fmt.Errorf("%w; it timed out waiting for a lock and applied nothing, retry once the blocking transaction ends", err)
}
