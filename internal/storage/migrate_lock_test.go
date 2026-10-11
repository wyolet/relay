package storage

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4/database"
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
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"value", database.Error{OrigErr: timeout}, true},
		{"pointer", fmt.Errorf("wrapped: %w", &database.Error{OrigErr: timeout}), true},
		{"bare", timeout, true},
		{"another sqlstate", database.Error{OrigErr: other}, false},
		{"not a postgres error", errors.New("boom"), false},
	} {
		if got := lockTimedOut(tc.err); got != tc.want {
			t.Errorf("%s: lockTimedOut = %v, want %v", tc.name, got, tc.want)
		}
	}
}
