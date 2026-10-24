package storage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestMigrationDSN(t *testing.T) {
	for _, tc := range []struct {
		name, dsn string
		timeout   time.Duration
		want      string
	}{
		{"sets the timeout in milliseconds", "postgres://u:p@db:5432/relay?sslmode=disable", 10 * time.Second, "postgres://u:p@db:5432/relay?lock_timeout=10000&sslmode=disable"},
		{"zero leaves the dsn alone", "postgres://u:p@db:5432/relay", 0, "postgres://u:p@db:5432/relay"},
		{"an operator's own value wins", "postgres://db/relay?lock_timeout=2s", 10 * time.Second, "postgres://db/relay?lock_timeout=2s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MigrationDSN(tc.dsn, tc.timeout)
			if err != nil {
				t.Fatalf("MigrationDSN: %v", err)
			}
			if got != tc.want {
				t.Fatalf("MigrationDSN = %q, want %q", got, tc.want)
			}
		})
	}
}

// Both drivers wrap the server's error in golang-migrate's non-unwrapping database.Error, by value from a migration body and by pointer elsewhere.
func TestLockTimedOut(t *testing.T) {
	timeout := &pgconn.PgError{Code: lockNotAvailable}
	other := &pgconn.PgError{Code: "42P07"}
	// Typed as error so vet accepts wrapping a pointer to a value-receiver error.
	var pointerErr error = &database.Error{OrigErr: timeout}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"value", database.Error{OrigErr: timeout}, true},
		{"pointer", fmt.Errorf("wrapped: %w", pointerErr), true},
		{"bare", timeout, true},
		{"another sqlstate", database.Error{OrigErr: other}, false},
		{"not a postgres error", errors.New("boom"), false},
	} {
		if got := lockTimedOut(tc.err); got != tc.want {
			t.Errorf("%s: lockTimedOut = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The lock-timeout error is logged; the migration body golang-migrate's error carries must not be.
func TestMigrationCauseDropsTheBody(t *testing.T) {
	const body = "ALTER TABLE providers ADD COLUMN note text;"
	var dbErr error = &database.Error{Query: []byte(body), OrigErr: &pgconn.PgError{Code: lockNotAvailable}}
	cause := migrationCause(fmt.Errorf("wrapped: %w", dbErr))
	if strings.Contains(cause.Error(), body) || !strings.Contains(cause.Error(), lockNotAvailable) {
		t.Fatalf("cause = %q, want the SQLSTATE and no migration body", cause)
	}
	var pg *pgconn.PgError
	if !errors.As(cause, &pg) {
		t.Fatalf("cause %v does not unwrap to the postgres error", cause)
	}
}

func TestMigrationFile(t *testing.T) {
	src, err := iofs.New(fstest.MapFS{
		"000032_add_note.up.sql":   {Data: []byte("SELECT 1;")},
		"000032_add_note.down.sql": {Data: []byte("SELECT 1;")},
	}, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		version uint
		up      bool
		want    string
	}{
		{32, true, "000032_add_note.up.sql"},
		{32, false, "000032_add_note.down.sql"},
		{33, true, "migration 33"},
	} {
		if got := migrationFile(src, tc.version, tc.up); got != tc.want {
			t.Errorf("migrationFile(%d, up=%v) = %q, want %q", tc.version, tc.up, got, tc.want)
		}
	}
}
